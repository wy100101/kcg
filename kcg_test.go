package main

import (
	"testing"
)

func TestLoadConfigSources(t *testing.T) {
	cfg, err := loadConfig("examples/kcg-simple.yaml")
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if len(cfg.Clusters) == 0 {
		t.Fatal("expected at least one cluster")
	}
	c := cfg.Clusters[0]
	sources := c.Sources()
	if len(sources) == 0 {
		t.Fatalf("expected non-empty sources, got empty map. raw sources field: %v", c.RawSources)
	}
	expected := map[string]string{
		"global":           "sources/global",
		"empty":            "sources/empty",
		"external-secrets": "sources/external-secrets",
	}
	for k, v := range expected {
		got, ok := sources[k]
		if !ok {
			t.Errorf("missing source key %q", k)
		} else if got != v {
			t.Errorf("source[%q] = %q, want %q", k, got, v)
		}
	}
	if len(sources) != len(expected) {
		t.Errorf("expected %d sources, got %d: %v", len(expected), len(sources), sources)
	}
}

func TestLoadConfigValues(t *testing.T) {
	cfg, err := loadConfig("examples/kcg-simple.yaml")
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	c := cfg.Clusters[0]
	values := c.Values()

	if values["platform"] != "cloud-01" {
		t.Errorf("platform = %q, want %q", values["platform"], "cloud-01")
	}
	if values["region"] != "us-central1" {
		t.Errorf("region = %q, want %q", values["region"], "us-central1")
	}

	sources, ok := values["sources"].(map[string]string)
	if !ok {
		t.Fatalf("values[sources] is %T, want map[string]string", values["sources"])
	}
	if len(sources) == 0 {
		t.Error("expected non-empty sources in values")
	}
}

func TestClusterSourcesDirectConstruction(t *testing.T) {
	c := cluster{
		Platform: "test",
		Region:   "us-east1",
		Env:      "dev",
		Cluster:  "test-cluster",
		RawSources: map[string]any{
			"foo": "bar",
			"baz": "qux",
			"nil": nil,
		},
	}
	sources := c.Sources()
	if len(sources) != 2 {
		t.Errorf("expected 2 sources (nil filtered out), got %d: %v", len(sources), sources)
	}
	if sources["foo"] != "bar" {
		t.Errorf("sources[foo] = %q, want %q", sources["foo"], "bar")
	}
	if sources["baz"] != "qux" {
		t.Errorf("sources[baz] = %q, want %q", sources["baz"], "qux")
	}
	if _, ok := sources["nil"]; ok {
		t.Error("expected nil source to be filtered out")
	}
}
