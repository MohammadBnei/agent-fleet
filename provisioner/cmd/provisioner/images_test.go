package main

import (
	"reflect"
	"testing"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
)

// images is the one place the Config becomes the k8s client's: a field added to k8s.Images and not set there
// compiles and reaches no pod. Every string field of a fully set Config is its own value, and every field of the
// result must be set.
func TestImages(t *testing.T) {
	var cfg config.Config
	cv := reflect.ValueOf(&cfg).Elem()
	for i := 0; i < cv.NumField(); i++ {
		cv.Field(i).SetString("v-" + cv.Type().Field(i).Name)
	}
	cfg.SessionNodeSelector = "k=v"
	img := images(cfg, "v-release")
	iv := reflect.ValueOf(img)
	for i := 0; i < iv.NumField(); i++ {
		if iv.Field(i).IsZero() {
			t.Errorf("Images.%s is not set from the Config", iv.Type().Field(i).Name)
		}
	}
	if img.Version != "v-release" || img.ClaudeModel != "v-ClaudeModel" || img.MaxTurns != "v-MaxTurns" || img.SessionNodeSelector["k"] != "v" {
		t.Errorf("%+v", img)
	}
}
