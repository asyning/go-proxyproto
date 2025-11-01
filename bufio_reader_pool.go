package proxyproto

import (
	"bufio"
	"io"
	"sync"
)

var bufReaderPool sync.Pool = sync.Pool{
	New: func() any {
		return bufio.NewReader(nil)
	},
}

func GetBufReader(w io.Reader) *bufio.Reader {
	v := bufReaderPool.Get()
	br := v.(*bufio.Reader)
	br.Reset(w)
	return br
}
func PutBufReader(br *bufio.Reader) {
	br.Reset(nil)
	bufReaderPool.Put(br)
}
