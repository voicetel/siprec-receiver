package rtp

import "testing"

// Reference expansion uses G.711 segment base/step tables independently
// of the production table-generation arithmetic, for all 256 codewords.
func TestG711ReferenceExpansion(t *testing.T) {
	uBase := [...]int{0, 132, 396, 924, 1980, 4092, 8316, 16764}
	aBase := [...]int{8, 264, 528, 1056, 2112, 4224, 8448, 16896}
	for code := range 256 {
		u := ^byte(code)
		segment := (u >> 4) & 7
		wantU := uBase[segment] + int(u&15)*(8<<segment)
		if u&128 != 0 {
			wantU = -wantU
		}
		a := byte(code) ^ 0x55
		segment = (a >> 4) & 7
		step := 16
		if segment > 0 {
			step <<= segment - 1
		}
		wantA := aBase[segment] + int(a&15)*step
		if a&128 == 0 {
			wantA = -wantA
		}
		out := make([]int16, 1)
		decodePCMUInto(out, []byte{byte(code)})
		if int(out[0]) != wantU {
			t.Errorf("PCMU %02x: got %d, want %d", code, out[0], wantU)
		}
		decodePCMAInto(out, []byte{byte(code)})
		if int(out[0]) != wantA {
			t.Errorf("PCMA %02x: got %d, want %d", code, out[0], wantA)
		}
	}
}
