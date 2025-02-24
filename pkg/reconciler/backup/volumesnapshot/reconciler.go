/*
Copyright The CloudNativePG Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package volumesnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	storagesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/strings/slices"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/log"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/resources"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/resources/instance"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// Reconciler is an object capable of executing a volume snapshot on a running cluster
type Reconciler struct {
	cli                  client.Client
	shouldFence          bool
	recorder             record.EventRecorder
	instanceStatusClient *instance.StatusClient
}

// ExecutorBuilder is a struct capable of creating a Reconciler
type ExecutorBuilder struct {
	executor Reconciler
}

// NewExecutorBuilder instantiates a new ExecutorBuilder with the minimum required data
func NewExecutorBuilder(
	cli client.Client,
	recorder record.EventRecorder,
) *ExecutorBuilder {
	return &ExecutorBuilder{
		executor: Reconciler{
			cli:                  cli,
			recorder:             recorder,
			instanceStatusClient: instance.NewStatusClient(),
		},
	}
}

// FenceInstance instructs if the Reconciler should fence or not the instance while taking the snapshot
func (e *ExecutorBuilder) FenceInstance(fence bool) *ExecutorBuilder {
	e.executor.shouldFence = fence
	return e
}

// Build returns the Reconciler instance
func (e *ExecutorBuilder) Build() *Reconciler {
	return &e.executor
}

func (se *Reconciler) enrichSnapshot(
	ctx context.Context,
	vs *storagesnapshotv1.VolumeSnapshot,
	backup *apiv1.Backup,
	cluster *apiv1.Cluster,
	targetPod *corev1.Pod,
) error {
	contextLogger := log.FromContext(ctx)
	snapshotConfig := *cluster.Spec.Backup.VolumeSnapshot

	vs.Labels[utils.BackupNameLabelName] = backup.Name

	switch snapshotConfig.SnapshotOwnerReference {
	case apiv1.SnapshotOwnerReferenceCluster:
		cluster.SetInheritedDataAndOwnership(&vs.ObjectMeta)
	case apiv1.SnapshotOwnerReferenceBackup:
		utils.SetAsOwnedBy(&vs.ObjectMeta, backup.ObjectMeta, backup.TypeMeta)
	default:
		break
	}

	// we grab the pg_controldata just before creating the snapshot
	if data, err := se.instanceStatusClient.GetPgControlDataFromInstance(ctx, targetPod); err == nil {
		vs.Annotations[utils.PgControldataAnnotationName] = data
	} else {
		contextLogger.Error(err, "while querying for pg_controldata")
	}

	rawCluster, err := json.Marshal(cluster)
	if err != nil {
		return err
	}

	vs.Annotations[utils.ClusterManifestAnnotationName] = string(rawCluster)

	return nil
}

// Execute the volume snapshot of the given cluster instance
func (se *Reconciler) Execute(
	ctx context.Context,
	cluster *apiv1.Cluster,
	backup *apiv1.Backup,
	targetPod *corev1.Pod,
	pvcs []corev1.PersistentVolumeClaim,
) (*ctrl.Result, error) {
	contextLogger := log.FromContext(ctx).WithValues("podName", targetPod.Name)

	// Step 1: fencing
	if se.shouldFence {
		contextLogger.Debug("Checking pre-requisites")
		if err := se.ensurePodIsFenced(ctx, cluster, backup, targetPod.Name); err != nil {
			return nil, err
		}

		if res, err := se.waitForPodToBeFenced(ctx, targetPod); res != nil || err != nil {
			return res, err
		}
	}

	// Step 2: create snapshot
	volumeSnapshots, err := GetBackupVolumeSnapshots(ctx, se.cli, cluster.Namespace, backup.Name)
	if err != nil {
		return nil, err
	}
	if len(volumeSnapshots) == 0 {
		// we execute the snapshots only if we don't find any
		if err := se.createSnapshotPVCGroupStep(ctx, cluster, pvcs, backup, targetPod); err != nil {
			return nil, err
		}

		// let's stop this reconciliation loop and wait for
		// the external snapshot controller to catch this new
		// request
		return &ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Step 3: wait for snapshots to be ready
	if res, err := se.waitSnapshotToBeReadyStep(ctx, volumeSnapshots); res != nil || err != nil {
		return res, err
	}

	if err := se.EnsurePodIsUnfenced(ctx, cluster, backup, targetPod); err != nil {
		return nil, err
	}

	return nil, nil
}

// ensurePodIsFenced checks if the preconditions for the execution of this step are
// met or not. If they are not met, it will return an error
func (se *Reconciler) ensurePodIsFenced(
	ctx context.Context,
	cluster *apiv1.Cluster,
	backup *apiv1.Backup,
	targetPodName string,
) error {
	fencedInstances, err := utils.GetFencedInstances(cluster.Annotations)
	if err != nil {
		return fmt.Errorf("could not check if cluster is fenced: %v", err)
	}

	if slices.Equal(fencedInstances.ToList(), []string{targetPodName}) {
		// We already requested the target Pod to be fenced
		return nil
	}

	if fencedInstances.Len() != 0 {
		return errors.New("cannot execute volume snapshot on a cluster that has fenced instances")
	}

	// The list of fenced instances is empty, so we need to request
	// fencing for the target pod
	se.recorder.Eventf(backup, "Normal", "FencePod",
		"Requesting fencing for Pod %v", targetPodName)

	if err := resources.ApplyFenceFunc(
		ctx,
		se.cli,
		cluster.Name,
		cluster.Namespace,
		targetPodName,
		utils.AddFencedInstance,
	); !errors.Is(err, utils.ErrorServerAlreadyFenced) {
		return err
	}
	return nil
}

// EnsurePodIsUnfenced removes the fencing status from the cluster
func (se *Reconciler) EnsurePodIsUnfenced(
	ctx context.Context,
	cluster *apiv1.Cluster,
	backup *apiv1.Backup,
	targetPod *corev1.Pod,
) error {
	contextLogger := log.FromContext(ctx)
	contextLogger.Info("Unfencing Pod")

	if err := resources.ApplyFenceFunc(
		ctx,
		se.cli,
		cluster.Name,
		cluster.Namespace,
		targetPod.Name,
		utils.RemoveFencedInstance,
	); err != nil {
		return err
	}

	// The list of fenced instances is empty, so we need to request
	// fencing for the target pod
	se.recorder.Eventf(backup, "Normal", "UnfencePod",
		"Un-fencing Pod %v", targetPod.Name)
	return nil
}

// waitForPodToBeFenced waits for the target Pod to be shut down
func (se *Reconciler) waitForPodToBeFenced(
	ctx context.Context,
	targetPod *corev1.Pod,
) (*ctrl.Result, error) {
	contextLogger := log.FromContext(ctx)

	var pod corev1.Pod
	err := se.cli.Get(ctx, types.NamespacedName{Name: targetPod.Name, Namespace: targetPod.Namespace}, &pod)
	if err != nil {
		return nil, err
	}
	ready := utils.IsPodReady(pod)
	if ready {
		contextLogger.Info("Waiting for target Pod to not be ready, retrying", "podName", targetPod.Name)
		return &ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return nil, nil
}

// snapshotPVCGroup creates a volumeSnapshot resource for every PVC
// used by the Pod
func (se *Reconciler) createSnapshotPVCGroupStep(
	ctx context.Context,
	cluster *apiv1.Cluster,
	pvcs []corev1.PersistentVolumeClaim,
	backup *apiv1.Backup,
	targetPod *corev1.Pod,
) error {
	snapshotSuffix := fmt.Sprintf("%d", time.Now().Unix())

	for i := range pvcs {
		se.recorder.Eventf(backup, "Normal", "CreateSnapshot",
			"Creating VolumeSnapshot for PVC %v", pvcs[i].Name)

		err := se.createSnapshot(ctx, cluster, backup, targetPod, &pvcs[i], snapshotSuffix)
		if err != nil {
			return err
		}
	}

	return nil
}

// waitSnapshotToBeReadyStep waits for every PVC snapshot to be ready to use
func (se *Reconciler) waitSnapshotToBeReadyStep(
	ctx context.Context,
	snapshots []storagesnapshotv1.VolumeSnapshot,
) (*ctrl.Result, error) {
	for i := range snapshots {
		if res, err := se.waitSnapshot(ctx, &snapshots[i]); res != nil || err != nil {
			return res, err
		}
	}

	return nil, nil
}

// createSnapshot creates a VolumeSnapshot resource for the given PVC and
// add it to the command status
func (se *Reconciler) createSnapshot(
	ctx context.Context,
	cluster *apiv1.Cluster,
	backup *apiv1.Backup,
	targetPod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	snapshotSuffix string,
) error {
	snapshotConfig := *cluster.Spec.Backup.VolumeSnapshot
	name := se.getSnapshotName(pvc.Name, snapshotSuffix)
	var snapshotClassName *string
	role := utils.PVCRole(pvc.Labels[utils.PvcRoleLabelName])
	if role == utils.PVCRolePgWal && snapshotConfig.WalClassName != "" {
		snapshotClassName = &snapshotConfig.WalClassName
	}

	// this is the default value if nothing else was assigned
	if snapshotClassName == nil && snapshotConfig.ClassName != "" {
		snapshotClassName = &snapshotConfig.ClassName
	}

	labels := pvc.Labels
	utils.MergeMap(labels, snapshotConfig.Labels)
	annotations := pvc.Annotations
	utils.MergeMap(annotations, snapshotConfig.Annotations)

	snapshot := storagesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   pvc.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: storagesnapshotv1.VolumeSnapshotSpec{
			Source: storagesnapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvc.Name,
			},
			VolumeSnapshotClassName: snapshotClassName,
		},
	}
	if snapshot.Labels == nil {
		snapshot.Labels = map[string]string{}
	}
	if snapshot.Annotations == nil {
		snapshot.Annotations = map[string]string{}
	}

	if err := se.enrichSnapshot(ctx, &snapshot, backup, cluster, targetPod); err != nil {
		return err
	}

	err := se.cli.Create(ctx, &snapshot)
	if err != nil {
		return fmt.Errorf("while creating VolumeSnapshot %s: %w", snapshot.Name, err)
	}

	return nil
}

// waitSnapshot waits for a certain snapshot to be ready to use
func (se *Reconciler) waitSnapshot(
	ctx context.Context,
	snapshot *storagesnapshotv1.VolumeSnapshot,
) (*ctrl.Result, error) {
	contextLogger := log.FromContext(ctx)

	info := parseVolumeSnapshotInfo(snapshot)
	if info.Error != nil {
		return nil, info.Error
	}
	if info.Running {
		contextLogger.Info(
			"Waiting for VolumeSnapshot to be ready to use",
			"volumeSnapshotName", snapshot.Name)
		return &ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return nil, nil
}

// getSnapshotName gets the snapshot name for a certain PVC
func (se *Reconciler) getSnapshotName(pvcName string, snapshotSuffix string) string {
	return fmt.Sprintf("%s-%s", pvcName, snapshotSuffix)
}
