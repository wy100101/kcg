# kcg
Kubernetes Configuration Generator

# Overview
A tool for generating Kubernetes configuration files

## why kcg

I wrote kcg when trying to figure out an easy way to administer the playform services for a fleet of kubernetes clusters via flux.

The requirements were:
- per cluster kustomization sources directories
- Write once manifests customized per cluster
