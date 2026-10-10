package prompt

import (
	"bufio"
	"os"
	"testing"
	"time"
)

func TestReadKeyReturnsLoneEscapeWithoutAnotherKey(t *testing.T) {
	reader, writer := keyPipe(t)
	if _, err := writer.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}

	keys := make(chan string, 1)
	go func() {
		key, _ := readKeyFrom(bufio.NewReader(reader), int(reader.Fd()))
		keys <- key
	}()
	select {
	case key := <-keys:
		if key != "\x1b" {
			t.Fatalf("readKey() = %q, want a lone Escape", key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a lone Escape blocked until another key arrived")
	}
}

func TestReadKeyKeepsEscapeSequencesWhole(t *testing.T) {
	for _, sequence := range []string{"a", "\x1b[A", "\x1b[3~", "\x1bOH", "\x1bc"} {
		reader, writer := keyPipe(t)
		if _, err := writer.Write([]byte(sequence)); err != nil {
			t.Fatal(err)
		}
		key, err := readKeyFrom(bufio.NewReader(reader), int(reader.Fd()))
		if err != nil || key != sequence {
			t.Fatalf("readKey(%q) = %q, %v", sequence, key, err)
		}
	}
}

func keyPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	return reader, writer
}
