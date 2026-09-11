package integrations

import "testing"

func TestParseNvidiaSMI(t *testing.T) {
	s, ok := parseNvidiaSMI("NVIDIA GeForce RTX 5080, 22, 100, 26, 5839, 16303, 37\n")
	if !ok {
		t.Fatal("parse failed")
	}
	if s.Name != "NVIDIA GeForce RTX 5080" || s.Util != 22 || s.Encoder != 100 || s.Decoder != 26 || s.Temp != 37 {
		t.Errorf("%+v", s)
	}
	if s.MemUsed != 5839<<20 || s.MemTotal != 16303<<20 || int(s.MemPercent()) != 35 {
		t.Errorf("memory: %+v %.1f", s, s.MemPercent())
	}
	if _, ok := parseNvidiaSMI("No devices were found"); ok {
		t.Error("garbage should not parse")
	}
}
