package imageverify

import (
	"encoding/json"
	"errors"
	"io"
)

// renderServiceInstallation is a pure preparation port for the owner-run setup
// wrapper. It creates no files or services and grants no authority. Root-owned
// installation and genuine runtime qualification remain separate prerequisites.
func renderServiceInstallation(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, serviceConfigMaxBytes+1))
	if err != nil || len(raw) > serviceConfigMaxBytes {
		return errors.New("service installation input is unbounded")
	}
	var c ServiceConfig
	if err = decodeClosedServiceObject(raw, &c); err != nil {
		return err
	}
	socket, service, err := RenderServiceUnits(c)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SchemaVersion int           `json:"schemaVersion"`
		Config        ServiceConfig `json:"config"`
		SocketUnit    string        `json:"socketUnit"`
		ServiceUnit   string        `json:"serviceUnit"`
	}{1, c, string(socket), string(service)})
}
