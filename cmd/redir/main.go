package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type dialerFunc = func(context.Context) (net.Conn, error)

type config struct {
	net, addr   string
	cnet, caddr string
}

func (cfg *config) dialer() dialerFunc {
	cnet, caddr := cfg.cnet, cfg.caddr

	return func(ctx context.Context) (net.Conn, error) {
		return net.Dial(cnet, caddr)
	}
}

func rootContext(ctx context.Context, logger *slog.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	procDone := make(chan os.Signal, 1)

	signal.Notify(procDone, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer cancel()

		ctxChan := ctx.Done()

		var procSignaled bool
		select {
		case <-procDone:
			// Likely an external process has signaled for a shutdown to happen gracefully.
			//
			// Technically a process may try to kill itself, but the normal thing is for the
			// context cancel func to be used for that case.
			procSignaled = true
		case <-ctxChan:
			// The context has either been cancelled due to a failure, expired due to a timeout
			// deadline being reached, or has naturally/gracefully come to its expected end.
		}

		if procSignaled {
			// Likely an external process has signaled for a shutdown to happen gracefully.
			//
			// Technically a process may try to kill itself, but the normal thing is for the
			// context cancel func to be used for that case.
			logger.WarnContext(ctx,
				"shutdown requested",
				slog.String("signaler", "process"),
			)

			return
		}

		// The context has either been cancelled due to a failure, expired due to a timeout
		// deadline being reached, or has naturally/gracefully come to its expected end.
		logger.WarnContext(ctx,
			"shutdown requested",
			slog.String("signaler", "context"),
			errAttr(ctx.Err()),
			slog.Any("cause", context.Cause(ctx)),
		)
	}()

	return ctx, cancel
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo

	if s := os.Getenv("LOG_LEVEL"); s != "" {
		var v slog.Level
		if err := v.UnmarshalText([]byte(s)); err != nil {
			panic(fmt.Errorf("failed to parse LOG_LEVEL env variable: %w", err))
		}
		level = v
	}

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})

	return slog.New(h)
}

func newConfig() config {

	laddr := "127.0.0.1"

	cfg := config{
		cnet: "tcp",
		net:  "tcp",
	}

	var icaddr string
	var lport, cport int
	var lportSet, cportSet bool

	usage := func(_ string) error {
		fmt.Println("")
		fmt.Println("#############")
		fmt.Println("##  redir  ##")
		fmt.Println("#############")
		fmt.Println("")
		fmt.Println("")

		flag.PrintDefaults()

		os.Exit(0)

		return nil
	}

	flag.StringVar(&cfg.net, "lnet", cfg.net, "listen network type, must be one of: "+strings.Join(allowedNets(), ", "))
	flag.StringVar(&laddr, "laddr", laddr, "address to liston on")
	flag.IntVar(&lport, "lport", lport, "port to listen on")
	flag.StringVar(&cfg.cnet, "cnet", cfg.cnet, "address to connect to")
	flag.StringVar(&icaddr, "caddr", icaddr, "connect to network type, must be one of: "+strings.Join(allowedNets(), ", "))
	flag.IntVar(&cport, "cport", cport, "port to connect to")
	flag.BoolFunc("help", "help", usage)
	flag.BoolFunc("usage", "help", usage)
	flag.BoolFunc("h", "help", usage)

	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "lport":
			lportSet = true
		case "cport":
			cportSet = true
		}
	})

	switch cfg.net {
	case "tcp", "tcp4", "tcp6":
		cfg.addr = net.JoinHostPort(laddr, strconv.Itoa(lport))
	case "unix", "unixpacket":
		if lportSet {
			panic("cannot specify lport on " + cfg.net + " network types")
		}
		cfg.addr = cfg.net + ":" + laddr
	default:
		panic("bad value for lnet: '" + cfg.net + "'")
	}

	if cport <= 0 {
		panic("bad value for cport: " + strconv.Itoa(cport))
	}

	switch cfg.cnet {
	case "tcp", "tcp4", "tcp6":
		cfg.caddr = net.JoinHostPort(icaddr, strconv.Itoa(cport))
	case "unix", "unixpacket":
		if cportSet {
			panic("cannot specify cport on " + cfg.cnet + " network types")
		}
		cfg.caddr = cfg.cnet + ":" + icaddr
	default:
		panic("bad value for cnet: '" + cfg.cnet + "'")
	}

	return cfg
}

func main() {

	logger := newLogger()

	var ctx context.Context
	{
		v, cancel := rootContext(context.Background(), logger)
		defer cancel()

		ctx = v
	}

	cfg := newConfig()

	logger.InfoContext(ctx,
		"starting listener",
		slog.String("addr", cfg.addr),
	)

	defer func() {
		logger.WarnContext(ctx,
			"stopped",
		)
	}()

	listener, err := net.Listen(cfg.net, cfg.addr)
	if err != nil {
		logger.ErrorContext(ctx,
			"failed to start listener",
			errAttr(err),
		)
		panic(err)
	}
	closeListener := listenerCloser(ctx, logger, listener)
	defer closeListener()

	dialer := cfg.dialer()

	serve(ctx, logger, listener, closeListener, dialer)
}

func listenerCloser(ctx context.Context, logger *slog.Logger, listener net.Listener) func() {

	closer := func() {
		if err := listener.Close(); err != nil {
			logger.ErrorContext(ctx,
				"failed to gracefully close listener",
				errAttr(err),
			)
			panic(err)
		}
	}

	return func() {
		if f := closer; f != nil {
			closer = nil
			f()
		}
	}
}

func serve(ctx context.Context, logger *slog.Logger, listener net.Listener, closeListener func(), dialer dialerFunc) {
	ctxChan := ctx.Done()

	logger.InfoContext(ctx,
		"starting handlers",
		slog.String("addr", listener.Addr().String()),
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			con, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) && done(ctxChan) {
					return
				}

				logger.DebugContext(ctx,
					"error accepting",
					errAttr(err),
				)

				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()

				if !handleCon(ctx, logger, dialer, con) {
					if err := con.Close(); err != nil {
						logger.ErrorContext(ctx,
							"failed to close 'from' connection",
							errAttr(err),
						)
					}
				}
			}()
		}
	}()

	defer wg.Wait()

	<-ctxChan

	logger.WarnContext(ctx,
		"stopping",
	)

	closeListener()
}

func handleCon(ctx context.Context, logger *slog.Logger, dialer dialerFunc, from net.Conn) bool {
	to, err := dialer(ctx)
	if err != nil {
		logger.ErrorContext(ctx,
			"failed to dial",
			errAttr(err),
		)
		return false
	}

	chan1 := make(chan struct{})
	go func() {
		defer close(chan1)

		for {
			if _, err := io.Copy(to, from); err != nil {
				if logger.Enabled(ctx, slog.LevelDebug) {
					logger.DebugContext(ctx,
						"copy errored",
						slog.String("operation", "from -> to"),
					)
				}
				return
			}
		}
	}()

	chan2 := make(chan struct{})
	go func() {
		defer close(chan2)

		for {
			if _, err := io.Copy(from, to); err != nil {
				if logger.Enabled(ctx, slog.LevelDebug) {
					logger.DebugContext(ctx,
						"copy errored",
						slog.String("operation", "from <- to"),
					)
				}
				return
			}
		}
	}()

	chans := [](<-chan struct{}){chan1, chan2}
	ctxChan := ctx.Done()

	defer func() {
		for _, v := range chans {
			<-v
		}
	}()

	select {
	case <-chans[0]:
		chans = chans[1:]
	case <-chans[1]:
		chans = chans[:1]
	case <-ctxChan:
		defer to.Close()
		from.Close()
	}

	return true
}

func allowedNets() []string {
	return []string{"tcp", "tcp4", "tcp6", "unix", "unixpacket"}
}

func errAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

func done(d <-chan struct{}) bool {
	select {
	case <-d:
		return true
	default:
		return false
	}
}
