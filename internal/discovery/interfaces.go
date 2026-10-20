package discovery

import (
	"net"
	"net/netip"
	"strings"
)

// 跳过的虚拟网卡名前缀（VPN、容器、虚拟机等）
var skipIfacePrefixes = []string{
	"utun", "bridge", "docker", "vethernet", "vmnet", "tailscale", "tun", "tap", "veth", "br-",
}

func shouldSkipIface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range skipIfacePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// eligibleInterfaces 返回可用于 LAN 发现的网卡：
// up、非 loopback、非 point-to-point、不在虚拟网卡黑名单中，且有单播地址。
func eligibleInterfaces() []*net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []*net.Interface
	for i := range ifaces {
		iface := &ifaces[i]
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if shouldSkipIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		out = append(out, iface)
	}
	return out
}

// ifaceAddrs 返回网卡的 IPv4 单播地址（netip）。
func ifaceAddrs(iface *net.Interface) []netip.Addr {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			if addr, ok := netip.AddrFromSlice(ip4); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out
}

// broadcastAddrs 返回应发送定向广播的子网广播地址（如 192.168.1.255）。
func broadcastAddrs(iface *net.Interface) []netip.Addr {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		mask4 := ipnet.Mask
		if ip4 == nil || len(mask4) != net.IPv4len {
			continue
		}
		bcast := make(net.IP, net.IPv4len)
		for i := 0; i < net.IPv4len; i++ {
			bcast[i] = ip4[i] | ^mask4[i]
		}
		// 跳过 /32 与全网广播（255.255.255.255 单独发送）
		ones, _ := net.IPMask(mask4).Size()
		if ones >= 31 {
			continue
		}
		if addr, ok := netip.AddrFromSlice(bcast); ok {
			out = append(out, addr.Unmap())
		}
	}
	return out
}
