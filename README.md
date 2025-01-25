# pktfwd

```
go mod init mir

$Env:GOOS = "linux"; $Env:GOARCH = "mips64"; go build ./


go clean -modcache
go build


//go:build linux && mips64 && !cgo
```
 
