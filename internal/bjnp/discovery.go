package bjnp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"
)

// discoveryTarget 记录可用 IPv4 网卡地址及其定向广播地址。
type discoveryTarget struct {
	local, broadcast *net.UDPAddr
}

// Discover 向每个活动 IPv4 网卡发送发现请求，在短窗口内按 MAC 去重收集设备。
func Discover(ctx context.Context) ([]Device, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var targets []discoveryTarget
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil || len(network.Mask) != net.IPv4len {
				continue
			}
			ip := network.IP.To4()
			broadcast := make(net.IP, net.IPv4len)
			for i := range broadcast {
				broadcast[i] = ip[i] | ^network.Mask[i]
			}
			targets = append(targets, discoveryTarget{
				local:     &net.UDPAddr{IP: ip, Port: 8611},
				broadcast: &net.UDPAddr{IP: broadcast, Port: 8611},
			})
		}
	}
	return discoverTargets(ctx, targets)
}

// discoveryResult 将各网卡独立收集的结果交给调用协程合并。
type discoveryResult struct {
	devices []Device
	err     error
}

// discoverTargets 同时收集各接口，避免接口数量延长广播等待窗口。
func discoverTargets(ctx context.Context, targets []discoveryTarget) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := make(chan discoveryResult, len(targets))
	for _, target := range targets {
		go func(target discoveryTarget) {
			devices, err := discoverOn(ctx, target)
			results <- discoveryResult{devices: devices, err: err}
		}(target)
	}
	devices := make(map[string]Device)
	var failures []error
	succeeded := 0
	for range targets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if result.err != nil {
				failures = append(failures, result.err)
			} else {
				succeeded++
			}
			for _, device := range result.devices {
				if previous, ok := devices[device.MAC]; !ok || device.Address < previous.Address {
					devices[device.MAC] = device
				}
			}
		}
	}
	if succeeded == 0 && len(failures) != 0 {
		return nil, errors.Join(failures...)
	}
	result := make([]Device, 0, len(devices))
	for _, device := range devices {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MAC < result[j].MAC })
	return result, nil
}

// discoverOn 使用单个接口发送一次无会话发现报文；无响应表示本窗口未发现设备。
func discoverOn(ctx context.Context, target discoveryTarget) ([]Device, error) {
	conn, err := net.ListenUDP("udp4", target.local)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(500 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	var request [headerSize]byte
	encodeHeader(request[:], cmdDiscover, 0, 0, 0)
	n, err := conn.WriteToUDP(request[:], target.broadcast)
	if err != nil {
		return nil, err
	}
	if n != len(request) {
		return nil, errors.New("BJNP 发现请求写入不完整")
	}
	var response [headerSize + maxResponse + 1]byte
	var devices []Device
	for {
		n, source, err := conn.ReadFromUDP(response[:])
		if err != nil {
			if ctx.Err() != nil {
				return devices, ctx.Err()
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				return devices, nil
			}
			return devices, err
		}
		device, err := parseDiscovery(response[:n], source)
		if err == nil {
			devices = append(devices, device)
		}
	}
}

// parseDiscovery 验证发现响应布局，使用实际报文来源而非未经验证的负载地址。
func parseDiscovery(data []byte, source *net.UDPAddr) (Device, error) {
	if len(data) < headerSize {
		return Device{}, errors.New("BJNP 发现响应不完整")
	}
	response, length, err := decodeHeader(data[:headerSize], maxResponse)
	if err != nil {
		return Device{}, err
	}
	if err := response.match(cmdDiscover, 0, 0); err != nil {
		return Device{}, err
	}
	if length != len(data)-headerSize || length < 6 {
		return Device{}, errors.New("BJNP 发现响应长度无效")
	}
	payload := data[headerSize:]
	if payload[4] != 6 || payload[5] != 4 || length != 6+int(payload[4])+int(payload[5]) {
		return Device{}, errors.New("BJNP 发现响应 MAC/IP 长度无效")
	}
	if source == nil || source.IP.To4() == nil || source.IP.IsUnspecified() || source.IP.IsMulticast() {
		return Device{}, fmt.Errorf("BJNP 发现响应来源无效: %v", source)
	}
	return Device{Address: source.String(), MAC: net.HardwareAddr(payload[6:12]).String()}, nil
}
