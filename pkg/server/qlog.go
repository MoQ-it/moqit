package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// qlogTracer writes a qlog file per QUIC connection into dir, named
// <original destination connection ID>_<client|server>.sqlog.
func qlogTracer(dir string) func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return func(_ context.Context, isClient bool, id quic.ConnectionID) qlogwriter.Trace {
		side := "server"
		if isClient {
			side = "client"
		}
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s_%s.sqlog", id, side)))
		if err != nil {
			return nil
		}
		seq := qlogwriter.NewConnectionFileSeq(&bufferedFile{bufio.NewWriter(f), f}, isClient, id, []string{qlog.EventSchema})
		go seq.Run()
		return seq
	}
}

type bufferedFile struct {
	*bufio.Writer
	f *os.File
}

func (b *bufferedFile) Close() error {
	if err := b.Flush(); err != nil {
		b.f.Close()
		return err
	}
	return b.f.Close()
}
