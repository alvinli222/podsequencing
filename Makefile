# Makefile for Pod Sequence Controller

# Image URL to use all building/pushing image targets
IMG ?= pod-sequence-controller:latest
ACR ?= # set to your Azure Container Registry name (e.g., myregistry)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Deployment

.PHONY: install
install: ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/crd/podsequence-crd.yaml

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	kubectl delete -f config/crd/podsequence-crd.yaml

.PHONY: deploy
deploy: ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/rbac/rbac.yaml
	kubectl apply -f config/crd/podsequence-crd.yaml

.PHONY: deploy-image
deploy-image: ## Deploy controller and set image to $(IMG)
	kubectl apply -f config/rbac/rbac.yaml
	kubectl apply -f config/crd/podsequence-crd.yaml
	kubectl -n pod-sequence-system set image deploy/pod-sequence-controller manager=$(IMG)

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	kubectl delete -f config/rbac/rbac.yaml
	kubectl delete -f config/crd/podsequence-crd.yaml

.PHONY: deploy-example
deploy-example: ## Deploy example PodSequence
	kubectl apply -f config/samples/example-podsequence.yaml

.PHONY: clean-example
clean-example: ## Clean up example PodSequence
	kubectl delete -f config/samples/example-podsequence.yaml

##@ Azure

.PHONY: acr-build
acr-build: ## Build image remotely in Azure Container Registry via 'az acr build'. Set ACR=<registry name>.
	@if [ -z "$(ACR)" ]; then \
		echo "Error: ACR is not set. Usage: make acr-build ACR=<registry name>"; \
		exit 1; \
	fi
	az acr build --registry $(ACR) --image pod-sequence-controller:latest .

.PHONY: aks-attach-acr
aks-attach-acr: ## Attach ACR to AKS so the cluster can pull images. Requires AKS_NAME and AKS_RG.
	@if [ -z "$(AKS_NAME)" ] || [ -z "$(AKS_RG)" ] || [ -z "$(ACR)" ]; then \
		echo "Error: set AKS_NAME, AKS_RG, and ACR. Usage: make aks-attach-acr AKS_NAME=<name> AKS_RG=<rg> ACR=<registry>"; \
		exit 1; \
	fi
	az aks update -n $(AKS_NAME) -g $(AKS_RG) --attach-acr $(ACR)
