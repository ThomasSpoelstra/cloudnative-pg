# Labels and annotations

Resources in Kubernetes are organized in a flat structure, with no hierarchical
information or relationship between them. However, such resources and objects
can be linked together and put in relationship through **labels** and
**annotations**.

!!! info
    For more information, please refer to the Kubernetes documentation on
    [annotations](https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/) and
    [labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/).

In short:

- an annotation is used to assign additional non-identifying information to
  resources with the goal to facilitate integration with external tools
- a label is used to group objects and query them through Kubernetes' native
  selector capability

You can select one or more labels and/or annotations you will use
in your CloudNativePG deployments. Then you need to configure the operator
so that when you define these labels and/or annotations in a cluster's metadata,
they are automatically inherited by all resources created by it (including pods).

!!! Note
    Label and annotation inheritance is the technique adopted by CloudNativePG
    in lieu of alternative approaches such as pod templates.

## Predefined labels

Below is a list of predefined labels that are managed by CloudNativePG.

`cnpg.io/backupName`
:   Backup identifier, only available on `Backup` and `VolumeSnapshot`
    resources

`cnpg.io/cluster`
:   Name of the cluster

`cnpg.io/immediateBackup`
:   Applied to a `Backup` resource if the backup is the first one created from
    a `ScheduledBackup` object having `immediate` set to `true`.

`cnpg.io/instanceName`
:   Name of the PostgreSQL instance - this label replaces the old and
    deprecated `postgresql` label

`cnpg.io/jobRole`
:   Role of the job (i.e. `import`, `initdb`, `join`, ...)

`cnpg.io/podRole`
:   Currently fixed to `instance` to identify a pod running PostgreSQL

`cnpg.io/poolerName`
:   Name of the PgBouncer pooler

`cnpg.io/pvcRole`
:   Purpose of the PVC, such as `PG_DATA` or `PG_WAL`

`cnpg.io/reload`
:   Available on `ConfigMap` and `Secret` resources. When set to `true`,
    a change in the resource will be automatically reloaded by the operator.

`cnpg.io/scheduled-backup`
:   When available, name of the `ScheduledBackup` resource that created a given
    `Backup` object.

`role`
:   Whether the instance running in a pod is a `primary` or a `replica`

## Predefined annotations

Below is a list of predefined annotations that are managed by CloudNativePG.

`container.apparmor.security.beta.kubernetes.io/*`
:   Name of the AppArmor profile to apply to the named container.
    See [AppArmor](security.md#restricting-pod-access-using-apparmor)
    documentation for details

`cnpg.io/coredumpFilter`
:   Filter to control the coredump of Postgres processes, expressed with a
    bitmask. By default it is set to `0x31` in order to exclude shared memory
    segments from the dump. Please refer to ["PostgreSQL core dumps"](troubleshooting.md#postgresql-core-dumps)
    for more information.

`cnpg.io/clusterManifest`
:   Manifest of the `Cluster` owning this resource (such as a PVC) - this label
    replaces the old and deprecated `cnpg.io/hibernateClusterManifest` label

`cnpg.io/fencedInstances`
:   List, expressed in JSON format, of the instances that need to be fenced.
    The whole cluster is fenced if the list contains the `*` element.

`cnpg.io/forceLegacyBackup`
:   Applied to a `Cluster` resource for testing purposes only, in order to
    simulate the behavior of `barman-cloud-backup` prior to version 3.4 (Jan 2023)
    when the `--name` option was not available.

`cnpg.io/hash`
:   The hash value of the resource

`cnpg.io/hibernation`
:   Applied to a `Cluster` resource to control the [declarative hibernation feature](declarative_hibernation.md).
    Allowed values are `on` and `off`.

`cnpg.io/managedSecrets`
:   Pull secrets managed by the operator and automatically set in the
    `ServiceAccount` resources for each Postgres cluster

`cnpg.io/nodeSerial`
:   On a pod resource, identifies the serial number of the instance within the
    Postgres cluster

`cnpg.io/operatorVersion`
:   Version of the operator

`cnpg.io/pgControldata`
:   Output of the `pg_controldata` command - this annotation replaces the old and
    deprecated `cnpg.io/hibernatePgControlData` annotation

`cnpg.io/podEnvHash`
:   *Deprecated* as the `cnpg.io/podSpec` annotation now also contains the pod environment

`cnpg.io/podSpec`
:   Snapshot of the `spec` of the Pod generated by the operator - this annotation replaces
    the old and deprecated `cnpg.io/podEnvHash` annotation

`cnpg.io/poolerSpecHash`
:   Hash of the pooler resource

`cnpg.io/pvcStatus`
:   Current status of the pvc, one of `initializing`, `ready`, `detached`

`cnpg.io/reconciliationLoop`
:   When set to `disabled` on a `Cluster`, the operator prevents the
    reconciliation loop from running

`cnpg.io/reloadedAt`
:   Contains the latest cluster `reload` time, `reload` is triggered by user through plugin

`cnpg.io/skipEmptyWalArchiveCheck`
:   When set to `true` on a `Cluster` resource, the operator disables the check
    that ensures that the WAL archive is empty before writing data. Use at your own
    risk.

`kubectl.kubernetes.io/restartedAt`
:  When available, the time of last requested restart of a Postgres cluster

## Pre-requisites

By default, no label or annotation defined in the cluster's metadata is
inherited by the associated resources.
In order to enable label/annotation inheritance, you need to follow the
instructions provided in the ["Operator configuration"](operator_conf.md) section.

Below we will continue on that example and limit it to the following:

- annotations: `categories`
- labels: `app`, `environment`, and `workload`

!!! Note
    Feel free to select the names that most suit your context for both
    annotations and labels. Remember that you can also use wildcards
    in naming and adopt strategies like `mycompany/*` for all labels
    or annotations starting with `mycompany/` to be inherited.

## Defining cluster's metadata

When defining the cluster, **before** any resource is deployed, you can
properly set the metadata as follows:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: cluster-example
  annotations:
    categories: database
  labels:
    environment: production
    workload: database
    app: sso
spec:
     # ... <snip>
```

Once the cluster is deployed, you can verify, for example, that the labels
have been correctly set in the pods with:

```shell
kubectl get pods --show-labels
```

## Current limitations

Currently, CloudNativePG does not automatically propagate labels or
annotations deletions. Therefore, when an annotation or label is removed from
a Cluster, which was previously propagated to the underlying pods, the operator
will not automatically remove it on the associated resources.
