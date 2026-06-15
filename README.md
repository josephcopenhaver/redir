# redir

[![Go Report Card](https://goreportcard.com/badge/github.com/josephcopenhaver/redir)](https://goreportcard.com/report/github.com/josephcopenhaver/redir)

Just a golang clone of the basic packet forwarding capabilities of [redir](https://github.com/TracyWebTech/redir) which was linked to from [homebrew](https://formulae.brew.sh/formula/redir).

---

```text
00:~ $ redir --help

#############
##         ##
##  redir  ##
##         ##
#############


  -caddr string
        address to connect to
  -cnet string
        connect to network type, must be one of: tcp, tcp4, tcp6, unix (default "tcp")
  -cport int
        port to connect to
  -dial-timeout string
        see docs at https://pkg.go.dev/time#ParseDuration (default "5s")
  -h    help
  -help
        help
  -laddr string
        address to liston on (default "127.0.0.1")
  -lnet string
        listen network type, must be one of: tcp, tcp4, tcp6, unix (default "tcp")
  -lport int
        port to listen on
  -usage
        help
  -v    version
  -version
        version
00:~ $
```

---

[![Go Reference](https://pkg.go.dev/badge/github.com/josephcopenhaver/redir.svg)](https://pkg.go.dev/github.com/josephcopenhaver/redir)
