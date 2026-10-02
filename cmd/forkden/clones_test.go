package main

import (
	"bytes"
	"encoding/json"
	"testing"

	v1 "github.com/forkden/cli/api/v1"
)

func TestCloneCommandsSendTypedMetadataOnly(t *testing.T) {
	t.Parallel()
	for _, item := range []struct {
		args   []string
		action v1.Action
		want   string
	}{
		{[]string{"clone", "create", "commerce", "--source", "sample"}, v1.CloneCreate, `{"name":"commerce","source":"sample","target_env":"FORKDEN_TARGET_URL"}`},
		{[]string{"clone", "list"}, v1.CloneList, `{}`},
		{[]string{"clone", "show", "commerce"}, v1.CloneShow, `{"clone":"commerce"}`},
		{[]string{"clone", "refresh", "commerce"}, v1.CloneRefresh, `{"clone":"commerce"}`},
		{[]string{"clone", "remove", "cl_example"}, v1.CloneRemove, `{"clone":"cl_example"}`},
		{[]string{"clone", "prune"}, v1.ClonePrune, `{}`},
		{[]string{"fork", "create", "--clone", "commerce", "--version", "2", "--ttl", "30m"}, v1.ForkCreate, `{"clone":"commerce","version":2,"ttl_seconds":1800}`},
		{[]string{"fork", "create", "--source", "sample"}, v1.ForkCreate, `{"source":"sample","target_env":"FORKDEN_TARGET_URL","ttl_seconds":3600}`},
	} {
		client := &fakeEngine{}
		_, err := dispatch(t.Context(), client, item.args, &bytes.Buffer{})
		payload, marshalErr := json.Marshal(client.input)
		if err != nil || marshalErr != nil || client.calls != 1 || client.action != item.action || string(payload) != item.want {
			t.Errorf("dispatch(%v) = action %s, input %s, error %v; want %s %s", item.args, client.action, payload, err, item.action, item.want)
		}
	}
}

func TestInvalidCloneArgumentsDoNotStartEngineWork(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"clone", "create", "commerce"}, {"clone", "list", "extra"}, {"clone", "remove"}, {"clone", "refresh", "commerce", "extra"},
		{"fork", "create"}, {"fork", "create", "--source", "sample", "--clone", "commerce"},
		{"fork", "create", "--source", "sample", "--version", "1"}, {"fork", "create", "--clone", "commerce", "--version", "-1"},
		{"fork", "create", "--clone", "commerce", "--target_env", "OTHER_TARGET"},
	} {
		client := &fakeEngine{}
		_, err := dispatch(t.Context(), client, args, &bytes.Buffer{})
		if exitCode(err) != 2 || client.calls != 0 {
			t.Errorf("dispatch(%v) = %v, calls %d; want input rejection without engine call", args, err, client.calls)
		}
	}
}
