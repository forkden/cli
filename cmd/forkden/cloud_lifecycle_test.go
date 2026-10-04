package main

import (
	"strings"
	"testing"
)

func TestCloudLifecycleUsesResourceIdentityAndPinnedVersion(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	for _, args := range [][]string{
		{"--api_url", f.server.URL, "login", "--no-browser"},
		{"cloud", "project", "use", "prj_demo", "--org", "org_demo"},
		{"cloud", "clone", "versions", "res_demo"},
		{"cloud", "fork", "create", "Pinned", "--clone", "res_demo", "--version", "2", "--request_id", "pinned-version-00001"},
		{"cloud", "clone", "refresh", "res_demo", "--request_id", "capture-version-00001"},
		{"cloud", "fork", "close", "res_demo", "--request_id", "close-same-fork-00001", "--wait"},
		{"cloud", "clone", "delete", "res_demo", "--request_id", "delete-clone-00001"},
	} {
		if _, _, err := f.run(t, args...); err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.intents) != 4 || f.intents[0].CloneVersion != 2 {
		t.Fatal("chosen snapshot version was not preserved")
	}
	for i, kind := range []string{"clone.refresh", "fork.close", "clone.delete"} {
		in := f.intents[i+1]
		if in.Kind != kind || in.ResourceID != "res_demo" || in.Name != "" || in.ProfileID != "" || in.CloneID != "" || in.CloneVersion != 0 || in.TTLSeconds != 0 {
			t.Fatal("lifecycle request supplied new provenance or omitted resource identity")
		}
	}
	if f.intents[2].IdempotencyKey != "close-same-fork-00001" || f.jobReads < 2 {
		t.Fatal("close did not preserve retry identity and wait for worker completion")
	}
}

func TestCloudLifecycleRejectsPrivateFlagsAndInvalidVersionsBeforeMutation(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	for _, args := range [][]string{
		{"--api_url", f.server.URL, "login", "--no-browser"},
		{"cloud", "project", "use", "prj_demo", "--org", "org_demo"},
	} {
		if _, _, err := f.run(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"cloud", "clone", "delete", "res_demo", "--target_env", "PRIVATE_DB"},
		{"cloud", "fork", "close", "res_demo", "--clone", "res_other"},
		{"cloud", "clone", "refresh", "res_demo", "--version", "1"},
		{"cloud", "fork", "create", "Bad", "--clone", "res_demo", "--version", "0"},
		{"cloud", "fork", "create", "Bad", "--clone", "res_demo", "--version", "10001"},
	} {
		if _, _, err := f.run(t, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.intents) != 0 {
		t.Fatal("invalid CLI request reached mutation API")
	}
}

func TestCloudWaitTreatsCancellingAsNonterminal(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	f.readStatuses = []string{"cancelling", "cancelled"}
	for _, args := range [][]string{
		{"--api_url", f.server.URL, "login", "--no-browser"},
		{"cloud", "project", "use", "prj_demo", "--org", "org_demo"},
	} {
		if _, _, err := f.run(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, progress, err := f.run(t, "cloud", "job", "wait", "job_demo")
	if err == nil || !strings.Contains(progress, "cancelling") || !strings.Contains(progress, "cancelled") {
		t.Fatal("wait ended before the worker confirmed cancellation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobReads != 2 {
		t.Fatal("cancelling was treated as terminal")
	}
}
