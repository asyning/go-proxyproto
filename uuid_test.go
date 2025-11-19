package proxyproto

import "testing"

func BenchmarkUUID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		NewUUIDV4()
		//NewUUIDV1()
		//NewUUIDV6()
		//NewUUIDV7()
	}
}
