# Storage support matrix

Treebind syncs files through a PVC, so how well it works depends on the storage
behind that PVC: locking, atomic rename, file events and the executable bit all
differ between local disks, NFS and cloud file systems.

**Status: not measured yet.** No storage class has been tested, so nothing is
listed as supported. The matrix below fills in as the conformance suite lands
and as people report results.

| Storage | Access mode | Result | Treebind version | Date | Tested on | Report |
| :-- | :-- | :-- | :-- | :-- | :-- | :-- |
| local-path | ReadWriteOnce | Not tested | | | | |
| NFS (in-cluster) | ReadWriteMany | Not tested | | | | |
| CephFS / RBD | | Not tested | | | | |
| AWS EFS / EBS | | Not tested | | | | |
| Azure Files / Disk | | Not tested | | | | |
| GCP Filestore / PD | | Not tested | | | | |

## Status values

- **Supported**: The conformance suite has successfully passed on that storage
  class. This is the only condition under which a storage class may be marked
  Supported.
- **Partial**: Some checks passed, but others failed. The row must explicitly
  identify which checks failed.
- **Failed**: The conformance run failed.
- **Not tested**: No valid conformance result is available.

A storage class is never advertised as Supported without a passing conformance
run. Other evidence, benchmarks or manual checks do not qualify.

## Report a result

Once the suite exists (`make conformance STORAGE_CLASS=<name>`), run it against
a cluster that uses your storage class and open a
[storage conformance report](https://github.com/mohanedmoh/treebind/issues/new?template=conformance_report.yml).
Please remove credentials and private hostnames from the output first.

Community results arrive through that form. Use the date the report issue was
opened as the `Date` value, and link that report issue in the `Report` column so
anyone can check the result. The form has no separate date field; the report
issue is the source of the date.
