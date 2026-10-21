package main

import "testing"

func TestRTPBindConfiguration(t *testing.T) {
	t.Setenv("SIPREC_LOCAL_IP", "203.0.113.7")
	t.Setenv("SIPREC_RTP_BIND_IP", "")
	cfg, err := loadConfig()
	if err != nil || cfg.RTPBindIP != "203.0.113.7" {
		t.Fatalf("default bind: %+v, %v", cfg, err)
	}
	t.Setenv("SIPREC_RTP_BIND_IP", "0.0.0.0")
	cfg, err = loadConfig()
	if err != nil || cfg.RTPBindIP != "0.0.0.0" || cfg.SIPLocalIP != "203.0.113.7" {
		t.Fatalf("separate bind: %+v, %v", cfg, err)
	}
	for _, invalid := range []string{"not-an-ip", "::1"} {
		t.Setenv("SIPREC_RTP_BIND_IP", invalid)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("accepted bind %q", invalid)
		}
	}
}
