package vmrt

import "testing"

func TestGuestMemoryOSReserve(t *testing.T) {
	cases := []struct {
		name             string
		total, osReserve int
		want             int
	}{
		{"linux 64 GB keeps a tenth", 65406, 0, 65406 - 6540},
		{"windows 64 GB keeps a tenth plus 4 GB", 65406, 4096, 65406 - 6540 - 4096},
		{"windows 16 GB keeps the 4 GB floor plus 4 GB", 16384, 4096, 16384 - 4096 - 4096},
		{"windows 8 GB keeps the floor plus 4 GB: too small", 8192, 4096, 0},
	}
	for _, c := range cases {
		got := Spec{TotalMemMB: c.total, OSReserveMB: c.osReserve}.GuestMemoryMB()
		if got != c.want {
			t.Errorf("%s: GuestMemoryMB() = %d, want %d", c.name, got, c.want)
		}
	}
}
