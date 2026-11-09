package ladder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// pinnedDefinitions are the sha256 of every released workload's definition.
// A changed definition fails here: give it a new version (major when results
// no longer compare, minor when they still do), and add the new entry; a
// released entry is never edited or removed.
var pinnedDefinitions = map[string]string{
	"max-prefill/1.0.0": "09674dae3022493ef9239e343d31a4c70b4397297895c4e86046a1cb65eb2bf3",
	"max-decode/1.0.0":  "c48c014657294717cd1fc631adbd72e2fc92cf57fa51db34fc12bbf5d8caebb6",
	"mixed-load/1.0.0":  "1929c4af22ec089edf159e2ed12f0597ceb1ea9411df1fe3b4107cfda94dbde4",
}

func definitionDigest(t *testing.T, workload Workload) string {
	t.Helper()
	encoded, err := json.Marshal(workload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func TestEveryWorkloadDefinitionIsPinned(t *testing.T) {
	for id, workload := range Workloads {
		if workload.ID != id {
			t.Errorf("workload %q is registered under %q", workload.ID, id)
		}
		pinned, ok := pinnedDefinitions[id]
		if !ok {
			t.Errorf("workload %s has no pinned definition: add %q", id, definitionDigest(t, workload))
			continue
		}
		if got := definitionDigest(t, workload); got != pinned {
			t.Errorf("workload %s changed (digest %s, pinned %s): a definition never changes without its version", id, got, pinned)
		}
	}
	for id := range pinnedDefinitions {
		if _, ok := Workloads[id]; !ok {
			t.Errorf("released workload %s was removed: a released workload stays measurable", id)
		}
	}
}

func TestAWorkloadIDIsANameAndASemanticVersion(t *testing.T) {
	for id := range Workloads {
		name, version, found := strings.Cut(id, "/")
		if !found || name == "" || len(strings.Split(version, ".")) != 3 {
			t.Errorf("workload id %q is not name/major.minor.patch", id)
		}
	}
}

func TestConfigForSetsEverythingFromTheWorkloadAndFitsTheContext(t *testing.T) {
	config, err := ConfigFor("mixed-load/1.0.0", 4096, 600e9, "test")
	if err != nil {
		t.Fatal(err)
	}
	if config.Workload != "mixed-load/1.0.0" || config.MinRequests != 20 || config.Seed != 1 || !config.IgnoreEOS {
		t.Fatalf("config %+v", config)
	}
	if config.PromptTokens != 128 || config.PromptTokensMax != 4096-1024-256 || config.AnswerTokensMin != 64 || config.AnswerTokens != 1024 {
		t.Fatalf("lengths %+v", config)
	}
	if got := config.Ladder(); len(got) != 16 || got[15] != 128 {
		t.Fatalf("ladder %v", got)
	}
	if _, err := ConfigFor("mixed/1.0.0", 0, 600e9, "test"); err == nil || !strings.Contains(err.Error(), "max-decode/1.0.0") {
		t.Fatalf("an unknown workload answered %v", err)
	}
}
