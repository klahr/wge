package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Network is a summary of one network, as the engine lists it.
type Network struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// CreateNetwork makes a private bridge network.
//
// Internal networks have no route off the host, which is what keeps a run's
// traffic inside the run. A game box that could reach the internet would be
// somebody's spam relay within the week, and the game itself never needs to.
func (d *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"Internal":       true,
		"CheckDuplicate": true,
		"Labels":         labels,
	}

	err := d.Post(ctx, "/networks/create", body, nil)
	if err != nil && isConflict(err) {
		// Another session created it first, which is the expected race when
		// two players' sessions start a run at the same moment.
		return nil
	}
	return err
}

func isConflict(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Status == 409
}

func asAPIError(err error, out **APIError) bool {
	ae, ok := err.(*APIError)
	if ok {
		*out = ae
	}
	return ok
}

// ConnectNetwork attaches a container to a network under the given aliases.
//
// The aliases are what make `ssh vault` work from another box in the run:
// Docker's embedded resolver answers them on this network and nowhere else.
func (d *Client) ConnectNetwork(ctx context.Context, network, container string, aliases []string) error {
	body := map[string]any{
		"Container": container,
		"EndpointConfig": map[string]any{
			"Aliases": aliases,
		},
	}

	err := d.Post(ctx, "/networks/"+network+"/connect", body, nil)
	if err != nil && isAlreadyConnected(err) {
		return nil
	}
	return err
}

func isAlreadyConnected(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	// The engine answers 403 for a container that is already an endpoint.
	return ae.Status == 403
}

// RemoveNetwork deletes a network. A network with endpoints still attached
// cannot be removed, which is why containers are reaped first.
func (d *Client) RemoveNetwork(ctx context.Context, name string) error {
	return d.Delete(ctx, "/networks/"+url.PathEscape(name))
}

// ListNetworks returns every network carrying the given label.
func (d *Client) ListNetworks(ctx context.Context, label string) ([]Network, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}

	var out []Network
	if err := d.Get(ctx, "/networks?filters="+url.QueryEscape(string(filters)), &out); err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	return out, nil
}
