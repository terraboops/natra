#!/bin/bash
set -x

# Install kind for local K8s testing
curl -Lo ./kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64
chmod +x ./kind
mv ./kind /usr/local/bin/kind

# Install kubectl
KUBECTL_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
curl -LO "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
chmod +x kubectl
mv kubectl /usr/local/bin/kubectl

# Create kind network
docker network create -d=bridge --subnet=172.19.0.0/24 kind || true

# Verify installations
kind version
docker --version
go version
kubectl version --client
