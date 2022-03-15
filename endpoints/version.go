package endpoints

import (
	"encoding/json"
	"net/http"

	log "github.com/sirupsen/logrus"
)

const versionEndpointValueNotSet = "not-set"

// NewVersionEndpoint returns the latest git tag as the version and commit hash as the revision from which the binary was built
func NewVersionEndpoint(version string) http.HandlerFunc {
	if version == "" {
		version = versionEndpointValueNotSet
	}
	response, err := json.Marshal(struct {
		Version string `json:"version"`
	}{
		Version: version,
	})
	if err != nil {
		log.Fatalf("error creating /version endpoint response: %v", err)
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Write(response)
	}
}
