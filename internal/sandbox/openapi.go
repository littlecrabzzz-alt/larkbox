package sandbox

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openAPISource []byte

var openAPIDocument = openAPIJSON(openAPISource)

func openAPIJSON(source []byte) json.RawMessage {
	var document map[string]any
	if err := yaml.Unmarshal(source, &document); err != nil {
		panic("invalid embedded sandbox OpenAPI document: " + err.Error())
	}
	output, err := json.Marshal(document)
	if err != nil {
		panic("invalid embedded sandbox OpenAPI document: " + err.Error())
	}
	return output
}

func describeOpenAPI(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, openAPIDocument)
}
