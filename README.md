## Amazon EKS Jobs Cleaner
Kubernetes has released the feature gate [TTLAfterFinished](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/) in `v1.12`.<br/>
This feature gate allows automatic garbage collection of jobs by specifying the `.spec.ttlSecondsAfterFinished` field in the Job spec.<br/>
This feature being [alpha](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/), [AWS EKS does not support](https://github.com/aws/containers-roadmap/issues/255) it yet.

This utility cleans up completed jobs that are older than the `--days`.