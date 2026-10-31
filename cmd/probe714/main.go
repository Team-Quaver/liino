package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/team-quaver/liino"
	"github.com/team-quaver/liino/decode"
)

func main() {
	driver := flag.String("driver", "pipewire", "output driver")
	uri := flag.String("uri", "/tmp/q714.opus", "source")
	secs := flag.Int("secs", 10, "seconds to watch")
	flag.Parse()

	p, err := liino.New(liino.Config{
		Driver:        *driver,
		AppName:       "Quaver Music",
		StreamName:    "probe714",
		PrerollFrames: 48000 * 200 / 1000,
		Log:           func(s string) { fmt.Println("[log]", s) },
	})
	if err != nil {
		panic(err)
	}
	defer p.Close()
	p.SetMuted(true)

	events, cancel := p.Events(64)
	defer cancel()
	go func() {
		for ev := range events {
			fmt.Printf("[event] %s state=%s\n", ev.Kind, ev.Snapshot.State)
		}
	}()

	if err := p.Open(context.Background(), decode.Source{URI: *uri}, liino.OpenOptions{AutoPlay: true}); err != nil {
		panic(err)
	}
	start := time.Now()
	for i := 0; i < *secs*5; i++ {
		time.Sleep(200 * time.Millisecond)
		s := p.Snapshot()
		fmt.Printf("t=%5.1fs state=%-9s pos=%6s buf=%5s under=%d drops=%d frames=%d\n",
			time.Since(start).Seconds(), s.State, s.Position.Truncate(time.Millisecond),
			s.Buffered.Truncate(time.Millisecond), s.Underruns, s.Drops, s.FramesWritten)
	}
	if v := os.Getenv("FORCE_EXIT"); v != "" {
		os.Exit(0)
	}
}
