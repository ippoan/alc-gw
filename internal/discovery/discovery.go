// Package discovery は CoreS3 の GW 自動発見用 UDP ビーコン。
//
// GW と CoreS3 は同一 LAN にいる前提なので、operator に GW の IP を調べさせて
// `GW URL` を設定させる必要はない。GW が全 IPv4 インターフェースの
// ブロードキャストアドレスへ定期的に自分の WS ハブ URL を届ける:
//
//	UDP 9001 宛て、5 秒間隔:
//	  {"src":"alc-gw","type":"beacon","ws":"ws://192.168.11.5:9000","fw":"v0.1.5"}
//
// CoreS3 (alc-app-s3 の gw_link) は UDP 9001 を聴き、`GW URL` が NVS 未設定なら
// 見つけた URL へ自動接続する (NVS 設定は手動オーバーライドとして優先)。
// mDNS でなくブロードキャストなのは、ESP32 側が UDP ソケット 1 本で済み
// (mdns コンポーネント不要 = ヒープ・設定の追加なし)、同一セグメント前提の
// LAN 構成で十分なため。
package discovery

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"
)

// Interval はビーコン送信間隔。CoreS3 側の発見の実効遅延上限になる
const Interval = 5 * time.Second

type beacon struct {
	Src  string `json:"src"`
	Type string `json:"type"`
	Ws   string `json:"ws"`
	Fw   string `json:"fw"`
}

// Start は hubPort の WS ハブ URL を beaconPort へブロードキャストし続ける
// goroutine を起動する。
func Start(beaconPort, hubPort int, version string) {
	go func() {
		warned := false
		for {
			if err := broadcastAll(beaconPort, hubPort, version); err != nil && !warned {
				log.Printf("discovery: beacon 送信失敗 (以後のエラーは抑制): %v", err)
				warned = true
			}
			time.Sleep(Interval)
		}
	}()
}

// broadcastAll は全 IPv4 インターフェースのブロードキャストアドレスへ
// そのインターフェースの自 IP を載せた beacon を送る。
func broadcastAll(beaconPort, hubPort int, version string) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	var lastErr error
	sent := 0
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 ||
			iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			bcast := broadcastAddr(ip, ipnet.Mask)
			if bcast == nil {
				continue
			}
			payload, err := json.Marshal(beacon{
				Src:  "alc-gw",
				Type: "beacon",
				Ws:   fmt.Sprintf("ws://%s:%d", ip, hubPort),
				Fw:   version,
			})
			if err != nil {
				return err
			}
			if err := sendUDP(ip, bcast, beaconPort, payload); err != nil {
				lastErr = err
				continue
			}
			sent++
		}
	}
	if sent == 0 && lastErr != nil {
		return lastErr
	}
	return nil
}

// broadcastAddr は IPv4 の directed broadcast アドレス (ip | ^mask) を返す。
func broadcastAddr(ip net.IP, mask net.IPMask) net.IP {
	if len(mask) != net.IPv4len {
		mask = mask[len(mask)-net.IPv4len:]
	}
	out := make(net.IP, net.IPv4len)
	for i := 0; i < net.IPv4len; i++ {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}

// sendUDP は src インターフェースから bcast:port へ 1 パケット送る。
func sendUDP(src, bcast net.IP, port int, payload []byte) error {
	conn, err := net.DialUDP("udp4",
		&net.UDPAddr{IP: src},
		&net.UDPAddr{IP: bcast, Port: port})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(payload)
	return err
}
