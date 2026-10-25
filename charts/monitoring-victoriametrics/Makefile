HELM ?= helm

# values to override
IMAGE_PREFIX ?= ghcr.io/openeverest
IMAGE_NAME ?= monitoring-victoriametrics
VERSION ?= 0.1.0
CHART_VERSION ?= $(VERSION)

.PHONY: prepare-chart
prepare-chart:
	yq eval -i '.version = "$(CHART_VERSION)"' Chart.yaml
	yq eval -i '.appVersion = "$(VERSION)"' Chart.yaml
	yq eval -i '.image.repository = "$(IMAGE_PREFIX)/$(IMAGE_NAME)"' values.yaml
	yq eval -i '.image.tag = ""' values.yaml

.PHONY: release
release: prepare-chart
