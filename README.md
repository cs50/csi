# CSI Hostpath Driver

This repository hosts the CSI Hostpath driver and all of its build and dependent configuration files to deploy the driver.

## Deployment
Deployment varies depending on the Kubernetes version your cluster is running:
- [Deployment for Kubernetes 1.17 and later](docs/deploy-1.17-and-later.md)

## Examples
The following examples assume that the CSI hostpath driver has been deployed and validated:
- Volume snapshots
  - [Kubernetes 1.17 and later](docs/example-snapshots-1.17-and-later.md)

## Building the binaries
If you want to build the driver yourself, you can do so with the following command from the root directory:

```shell
make hostpath
```
