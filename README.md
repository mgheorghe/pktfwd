# pktfwd

```
go mod init mir

$Env:GOOS = "linux"; $Env:GOARCH = "mips64"; go build ./


go clean -modcache
go build


//go:build linux && mips64 && !cgo
```
1
```
/mir --src-ip 10.0.1.1 --dst-ip 10.0.1.8 --filter 946dae8b9b14 --filter-offset 6 &
/dan --src-ip 10.0.1.1 &
```

8
```
/mir --src-ip 10.0.1.8 --dst-ip 10.0.1.1 --filter 20010000000000000000000100010002 --filter-offset 38 &
/dan --src-ip 10.0.1.1 &
```


```
tcpreplay -i enp225s0np0  -K --pps 300000 --loop 1000000 --duration 10 ./mir.pcap

```
 
