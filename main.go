package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kardianos/imapdown/list"
	"github.com/kardianos/task"
)

func main() {
	err := task.Start(context.Background(), time.Second*2, run)
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	h := flag.String("host", "", "imap host:port")
	u := flag.String("user", "", "username")
	p := flag.String("pass", "", "password")
	s := flag.String("store", "", "dir to store email in")
	v := flag.Bool("verbose", false, "log events to std out")
	flag.Parse()
	if len(*h) == 0 {
		return fmt.Errorf("missing host")
	}
	if len(*s) == 0 {
		return fmt.Errorf("missing store")
	}
	err := os.MkdirAll(*s, 0700)
	if err != nil {
		return err
	}
	w := &list.Worker{
		Verbose: *v,
		Store:   *s,
	}
	return w.List(ctx, *h, *u, *p)
}
