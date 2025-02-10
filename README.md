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
/dan --src-ip 10.0.1.1 &
/mir --src-ip 10.0.1.8 --dst-ip 10.0.1.1 --filter 20010000000000000000000100010002 --filter-offset 38 --metrics
```


```
tcpreplay -i enp225s0np0  -K --pps 300000 --loop 1000000 --duration 10 ./mir.pcap

```
 


 Test via MGMT nic
1
 ```
/dan-mips64 --src-ip 10.0.1.1 --metrics
/mir-mips64 --src-ip 10.0.1.1 --dst-ip 10.0.0.2 --filter 946dae8b9b14 --filter-offset 6 --metrics
 ```

mgmt
 ```
/root/dan-amd64 --dst-iface mgmt0 --src-ip 10.0.1.1 --metrics
/root/mir-amd64 --src-iface mgmt0 --src-ip 10.0.0.2 --dst-ip 10.0.1.1 --filter 2620017b0003c000000000000004bd33 --filter-offset 38 --metrics



 ```

SNIC
```

ip addr add 2620:17b:3:c000::4:bd33/112 dev enp225s0np0

ip -6 route del 2620:17b:3:c000::4:0/112 dev br1
ip -6 route add 2620:17b:3:c000::4:0/112 dev enp225s0np0

ip -6 neigh del 2620:17b:3:c000::4:1 dev enp225s0np0
ip -6 neigh add 2620:17b:3:c000::4:1 lladdr a0:3d:6e:5c:d0:3f dev enp225s0np0
ip -6 n


ping6 2620:17b:3:c000::4:1 -I enp225s0np0




```

```
scp /root/mir-mips64 10.0.1.1:/
scp /root/dan-mips64 10.0.1.1:/


ssh 10.0.1.1

chmod 777 /dan-mips64
chmod 777 /mir-mips64


```



Build:

```
cd ../dan
$Env:GOOS = "linux"; $Env:GOARCH = "amd64"; go build -o dan-amd64 ./
$Env:GOOS = "linux"; $Env:GOARCH = "mips64"; go build -o dan-mips64 ./

cd ../mir
$Env:GOOS = "linux"; $Env:GOARCH = "amd64"; go build -o mir-amd64 ./
$Env:GOOS = "linux"; $Env:GOARCH = "mips64"; go build -o mir-mips64 ./


```