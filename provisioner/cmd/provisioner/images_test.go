package main

import (
	"reflect"
	"testing"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
)

// images is the one place the Config becomes the k8s client's: a field added to k8s.Images and not set there
// compiles and reaches no pod, and two fields swapped there compile and reach the wrong one. Every field of a
// fully set Config holds its own name, so each Images field must hold the name of the Config field it comes
// from, spelled out in the table below.
func TestImages(t *testing.T) {
	var cfg config.Config
	cv := reflect.ValueOf(&cfg).Elem()
	for i := 0; i < cv.NumField(); i++ {
		cv.Field(i).SetString("v-" + cv.Type().Field(i).Name)
	}
	cfg.SessionNodeSelector = "k=v"
	img := images(cfg, "v-release")
	iv := reflect.ValueOf(img)

	// Images field -> the Config field it is set from. Version is the release passed in, and
	// SessionNodeSelector the parsed form of the Config's single label.
	from := map[string]string{
		"WorkerImage":           "WorkerImage",
		"SidecarImage":          "SidecarImage",
		"WorkspacePVC":          "WorkspacePVC",
		"SessionStorageClass":   "SessionStorageClass",
		"LogLevel":              "LogLevel",
		"CoreGRPCAddr":          "CoreGRPCAddr",
		"ThotAuthToken":         "ThotAuthToken",
		"ExecutorAddr":          "ExecutorAddr",
		"PostgresImage":         "PostgresImage",
		"RedisImage":            "RedisImage",
		"SharedInstancePVCSize": "SharedInstancePVCSize",
		"GitURLRewrite":         "GitURLRewrite",
		"ClaudeModel":           "ClaudeModel",
		"MaxTurns":              "MaxTurns",
	}
	special := map[string]bool{"Version": true, "SessionNodeSelector": true}
	for i := 0; i < iv.NumField(); i++ {
		name := iv.Type().Field(i).Name
		if special[name] {
			continue
		}
		src, ok := from[name]
		if !ok {
			t.Errorf("Images.%s has no row in this test's table: say which Config field it comes from", name)
			continue
		}
		if got, want := iv.Field(i).String(), "v-"+src; got != want {
			t.Errorf("Images.%s = %q, want %q (Config.%s)", name, got, want, src)
		}
	}
	if img.Version != "v-release" || img.SessionNodeSelector["k"] != "v" || len(img.SessionNodeSelector) != 1 {
		t.Errorf("%+v", img)
	}
}
