package pdf

import (
	"compress/zlib"
	"io"
)

func newZlib(w io.Writer) io.WriteCloser { return zlib.NewWriter(w) }
