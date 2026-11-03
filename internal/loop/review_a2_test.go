package loop

import "testing"

func TestCaptureRetainsOnlyLimitPlusOneByte(t *testing.T) {
	c := Capture{Limit: MaxStream}
	block := make([]byte, 1<<20)
	for i := 0; i < 128; i++ {
		c.Write(block)
	}
	if !c.Over() || c.Len() != MaxStream+1 || cap(c.Bytes()) > MaxStream+1 {
		t.Fatalf("retained %d bytes", c.Len())
	}
}
