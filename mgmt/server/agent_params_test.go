package main

import (
	"reflect"
	"testing"
)

func TestAgentParamsDeviceURLs(t *testing.T) {
	d := &Device{}
	if _, ok := NewServer(&Config{}, nil).agentParams(nil, d)["mgmt_urls"]; ok {
		t.Fatal("mgmt_urls sent without device_urls configured")
	}
	urls := []string{"https://mgmt.example.net:9443", "https://192.0.2.1:9443"}
	srv := NewServer(&Config{DeviceURLs: urls}, nil)
	if got := srv.agentParams(nil, d)["mgmt_urls"]; !reflect.DeepEqual(got, urls) {
		t.Fatalf("default mgmt_urls = %v", got)
	}
	// A profile's telemetry section wins, as for every other agent parameter.
	tel := map[string]any{"mgmt_urls": []any{"https://other.example.net:9443"}}
	if got := srv.agentParams(tel, d)["mgmt_urls"]; !reflect.DeepEqual(got, tel["mgmt_urls"]) {
		t.Fatalf("telemetry mgmt_urls = %v", got)
	}
}
