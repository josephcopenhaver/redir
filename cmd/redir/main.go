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
	"time"
)

// note, build-time vars might move to their own ./internal/ sub package in the future

// build-time vars
var (
	version = "v1.1.8"
)

type dialerFunc = func(context.Context) (net.Conn, error)

type config struct {
	net, addr   string
	cnet, caddr string
	dialTimeout time.Duration
}

func (cfg *config) dialer() dialerFunc {
	cnet, caddr := cfg.cnet, cfg.caddr

	d := net.Dialer{
		Timeout: cfg.dialTimeout,
	}

	return func(ctx context.Context) (net.Conn, error) {

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return d.DialContext(ctx, cnet, caddr)
	}
}

func rootContext(ctx context.Context, logger *slog.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	procDone := make(chan os.Signal, 1)

	signal.Notify(procDone, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer cancel()

		ctxChan := ctx.Done()

		select {
		case <-procDone:
			// Likely an external process has signaled for a shutdown to happen gracefully.
			//
			// Technically a process may try to kill itself, but the normal thing is for the
			// context cancel func to be used for that case.
			logger.LogAttrs(ctx, slog.LevelWarn,
				"shutdown requested",
				slog.String("signaler", "process"),
			)
		case <-ctxChan:
			// The context has either been cancelled due to a failure, expired due to a timeout
			// deadline being reached, or has naturally/gracefully come to its expected end.
			logger.LogAttrs(ctx, slog.LevelWarn,
				"shutdown requested",
				slog.String("signaler", "context"),
				errAttr(ctx.Err()),
				slog.Any("cause", context.Cause(ctx)),
			)
		}
	}()

	return ctx, cancel
}

func newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo

	if s := os.Getenv("LOG_LEVEL"); s != "" {
		var v slog.Level
		if err := v.UnmarshalText([]byte(s)); err != nil {
			return nil, fmt.Errorf("failed to parse LOG_LEVEL env variable: %w", err)
		}
		level = v
	}

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})

	return slog.New(h), nil
}

func newConfig() (config, error) {
	var result config

	laddr := "127.0.0.1"

	cfg := config{
		cnet:        "tcp",
		net:         "tcp",
		dialTimeout: 5 * time.Second,
	}

	var icaddr, dialTimeout string
	var lport, cport int
	var lportSet, cportSet, dialTimeoutSet bool

	usage := func(string) error {
		fmt.Println("")
		fmt.Println("#############")
		fmt.Println("##         ##")
		fmt.Println("##  redir  ##")
		fmt.Println("##         ##")
		fmt.Println("#############")
		fmt.Println("")
		fmt.Println("")

		flag.PrintDefaults()

		os.Exit(0)

		return nil
	}

	printVersion := func(string) error {
		fmt.Println(version)

		os.Exit(0)

		return nil
	}

	flag.StringVar(&cfg.net, "lnet", cfg.net, "listen network type, must be one of: "+strings.Join(allowedNets(), ", "))
	flag.StringVar(&laddr, "laddr", laddr, "address to liston on")
	flag.IntVar(&lport, "lport", lport, "port to listen on")
	flag.StringVar(&cfg.cnet, "cnet", cfg.cnet, "connect to network type, must be one of: "+strings.Join(allowedNets(), ", "))
	flag.StringVar(&icaddr, "caddr", icaddr, "address to connect to")
	flag.IntVar(&cport, "cport", cport, "port to connect to")
	flag.StringVar(&dialTimeout, "dial-timeout", cfg.dialTimeout.String(), "see docs at https://pkg.go.dev/time#ParseDuration")
	flag.BoolFunc("help", "help", usage)
	flag.BoolFunc("usage", "help", usage)
	flag.BoolFunc("h", "help", usage)
	flag.BoolFunc("v", "version", printVersion)
	flag.BoolFunc("version", "version", printVersion)

	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "lport":
			lportSet = true
		case "cport":
			cportSet = true
		case "dial-timeout":
			dialTimeoutSet = true
		}
	})

	var lportUsed = true
	switch cfg.net {
	case "tcp", "tcp4", "tcp6":
		cfg.addr = net.JoinHostPort(laddr, strconv.Itoa(lport))
	case "unix", "unixpacket":
		lportUsed = false
		if lportSet {
			return result, fmt.Errorf("cannot specify lport on %s network types", cfg.net)
		}
		cfg.addr = cfg.net + ":" + laddr
	default:
		return result, fmt.Errorf("see help for valid lnet values: bad value")
	}

	if icaddr == "" {
		return result, fmt.Errorf("caddr cannot be empty")
	}

	var cportUsed = true
	switch cfg.cnet {
	case "tcp", "tcp4", "tcp6":
		cfg.caddr = net.JoinHostPort(icaddr, strconv.Itoa(cport))
	case "unix", "unixpacket":
		cportUsed = false
		if cportSet {
			return result, fmt.Errorf("cannot specify cport on %s network types", cfg.cnet)
		}
		cfg.caddr = cfg.cnet + ":" + icaddr
	default:
		return result, fmt.Errorf("see help for valid cnet values: bad value")
	}

	if cportSet {
		if cport <= 0 {
			return result, fmt.Errorf("cport must be greater than zero: bad value %d", cport)
		}
	} else if cportUsed {
		return result, fmt.Errorf("cport must be specified on %s network types", cfg.cnet)
	}

	if lportSet {
		if lport < 0 {
			return result, fmt.Errorf("lport must be greater than or equal to zero (note zero will make it random): bad value %d", lport)
		}
	} else if lportUsed {
		return result, fmt.Errorf("lport must be specified on %s network types", cfg.net)
	}

	if dialTimeoutSet {
		v, err := time.ParseDuration(dialTimeout)
		if err != nil {
			return result, fmt.Errorf("invalid dial timeout: %w", err)
		}

		cfg.dialTimeout = v
	}

	if cfg.dialTimeout <= 0 {
		return result, errors.New("invalid dial timeout: must be greater than zero")
	}

	result = cfg
	return result, nil
}

func main() {

	logger, err := newLogger()
	if err != nil {
		panic(fmt.Errorf("failed to create logger: %w", err))
	}

	var ctx context.Context
	{
		v, cancel := rootContext(context.Background(), logger)
		defer cancel()

		ctx = v
	}

	cfg, err := newConfig()
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError,
			"failed to create runtime configuration",
			errAttr(err),
		)
		panic(err)
	}

	dialer := cfg.dialer()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"starting listener",
		slog.String("addr", cfg.addr),
	)

	defer func() {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"stopped",
		)
	}()

	var listener net.Listener
	{
		v, err := net.Listen(cfg.net, cfg.addr)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelError,
				"failed to start listener",
				errAttr(err),
			)
			panic(err)
		}
		listener = v
	}

	serve(ctx, logger, listener, dialer)
}

func closeConnFunc(con net.Conn) func() {
	return sync.OnceFunc(func() {
		ignoredErr := con.Close()
		_ = ignoredErr
	})
}

func closeListenerFunc(ctx context.Context, logger *slog.Logger, listener net.Listener) func() {
	return sync.OnceFunc(func() {
		if err := listener.Close(); err != nil {
			logger.LogAttrs(ctx, slog.LevelError,
				"listener failed to close",
				slog.String("addr", listener.Addr().String()),
				errAttr(err),
			)
			return
		}

		logger.LogAttrs(ctx, slog.LevelWarn,
			"listener closed",
			slog.String("addr", listener.Addr().String()),
		)
	})
}

// serve starts goroutines to handle requests coming in to the listener.
func serve(ctx context.Context, logger *slog.Logger, listener net.Listener, dialer dialerFunc) {
	closeListener := closeListenerFunc(ctx, logger, listener)
	defer closeListener()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"starting handlers",
		slog.String("addr", listener.Addr().String()),
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// if this routine ever explodes, then all child contexts must terminate
		defer cancel()

		ctxChan := ctx.Done()

		next := func() bool {
			var con net.Conn
			var err error
			var closeFrom func()

			defer func() {
				if r := recover(); r == nil {
					return
				}

				const errMsg = "failed to start handler"

				if closeFrom == nil {
					if con == nil {
						return
					}

					attrs := make([]slog.Attr, 1, 2)
					attrs[0] = slog.String("remediation_performed", "closed socket")
					if err := con.Close(); err != nil {
						attrs = append(attrs, slog.Any("close_error", err))
					}

					logger.LogAttrs(ctx, slog.LevelError,
						errMsg,
						attrs...,
					)

					return
				}

				logger.LogAttrs(ctx, slog.LevelError,
					errMsg,
					slog.String("remediation_performed", "none"),
				)

				closeFrom()
			}()

			con, err = listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) && done(ctxChan) {
					return false
				}

				if logger.Enabled(ctx, slog.LevelDebug) {
					logger.LogAttrs(ctx, slog.LevelDebug,
						"error accepting",
						errAttr(err),
					)
				}

				return true
			}

			closeFrom = closeConnFunc(con)

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer closeFrom()

				handleCon(ctx, logger, dialer, con, closeFrom)
			}()

			return true
		}

		for next() {
		}
	}()

	defer wg.Wait()
	defer closeListener()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"ready",
	)

	<-ctx.Done()

	logger.LogAttrs(ctx, slog.LevelWarn,
		"stopping",
	)
}

func handleCon(ctx context.Context, logger *slog.Logger, dialer dialerFunc, from net.Conn, closeFrom func()) {

	logger.LogAttrs(ctx, slog.LevelDebug,
		"connection starting",
	)

	defer logger.LogAttrs(ctx, slog.LevelDebug,
		"connection closed",
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	to, err := dialer(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError,
			"failed to dial",
			errAttr(err),
		)
		return
	}
	closeTo := closeConnFunc(to)
	defer closeTo()

	var wg sync.WaitGroup
	defer wg.Wait()

	var duplexWG sync.WaitGroup
	duplexWG.Add(2)

	wg.Add(2)

	go func() {
		defer wg.Done()
		defer cancel()
		defer duplexWG.Wait()
		defer duplexWG.Done()

		if n, f := duplexCloser(to, from); f != nil {
			logger.LogAttrs(ctx, slog.LevelDebug,
				"will close duplex",
				slog.String("operation", "from -> to"),
				slog.Int("count", n),
			)

			defer f()

			defer logger.LogAttrs(ctx, slog.LevelDebug,
				"closing duplex",
				slog.String("operation", "from -> to"),
				slog.Int("count", n),
			)
		}

		if _, err := io.Copy(to, from); err != nil && logger.Enabled(ctx, slog.LevelDebug) {
			logger.LogAttrs(ctx, slog.LevelDebug,
				"copy errored",
				errAttr(err),
				slog.String("operation", "from -> to"),
				slog.String("from_remote_addr", addrToStr(from.RemoteAddr())),
				slog.String("from_local_addr", addrToStr(from.LocalAddr())),
			)
		}
	}()

	go func() {
		defer wg.Done()
		defer cancel()
		defer duplexWG.Wait()
		defer duplexWG.Done()

		if n, f := duplexCloser(from, to); f != nil {
			logger.LogAttrs(ctx, slog.LevelDebug,
				"will close duplex",
				slog.String("operation", "from <- to"),
				slog.Int("count", n),
			)

			defer f()

			defer logger.LogAttrs(ctx, slog.LevelDebug,
				"closing duplex",
				slog.String("operation", "from <- to"),
				slog.Int("count", n),
			)
		}

		if _, err := io.Copy(from, to); err != nil && logger.Enabled(ctx, slog.LevelDebug) {
			logger.LogAttrs(ctx, slog.LevelDebug,
				"copy errored",
				errAttr(err),
				slog.String("operation", "from <- to"),
				slog.String("from_remote_addr", addrToStr(from.RemoteAddr())),
				slog.String("from_local_addr", addrToStr(from.LocalAddr())),
			)
		}
	}()

	defer closeTo()
	defer closeFrom()

	defer logger.LogAttrs(ctx, slog.LevelDebug,
		"connection closing",
	)

	<-ctx.Done()
}

func allowedNets() []string {
	return []string{"tcp", "tcp4", "tcp6", "unix", "unixpacket"}
}

func errAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

// done is prematurely optimized for ~5k goroutines all trying to check the same ctx at the same time
//
// see https://stackoverflow.com/questions/57562606/why-does-sync-mutex-largely-drop-performance-when-goroutine-contention-is-more-t
//
// at lower scales `ctx.Err() != nil` is more performant
func done(d <-chan struct{}) bool {
	select {
	case <-d:
		return true
	default:
		return false
	}
}

func addrToStr(addr net.Addr) string {
	if addr == nil {
		return ""
	}

	return addr.String()
}

func duplexCloser(dst, src net.Conn) (int, func()) {
	var result func()
	var n int

	if v, ok := src.(*net.TCPConn); ok {
		n++
		result = func() {
			ignoredErr := v.CloseRead()
			_ = ignoredErr
		}
	} else if v, ok := src.(*net.UnixConn); ok {
		n++
		result = func() {
			ignoredErr := v.CloseRead()
			_ = ignoredErr
		}
	}

	{
		var closeW func() error
		if v, ok := dst.(*net.TCPConn); ok {
			n++
			closeW = v.CloseWrite
		} else if v, ok := dst.(*net.UnixConn); ok {
			n++
			closeW = v.CloseWrite
		}

		if closeW != nil {
			if f := result; f != nil {
				result = func() {
					defer f()
					ignoredErr := closeW()
					_ = ignoredErr
				}
			} else {
				result = func() {
					ignoredErr := closeW()
					_ = ignoredErr
				}
			}
		}
	}

	return n, result
}
