package core

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"ZJUConnect/core/config"

	"github.com/cxz66666/go-socks5"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type ZJUDnsResolve struct {
	remoteResolver *net.Resolver
}

func (resolve ZJUDnsResolve) Resolve(ctx context.Context, host string) (context.Context, net.IP, error) {
	if config.IsDnsRuleAvailable() {
		if ip, hasDnsRule := config.GetSingleDnsRule(host); hasDnsRule {
			ctx = context.WithValue(ctx, "USE_PROXY", true)
			log.Printf("%s -> %s", host, ip)
			return ctx, net.ParseIP(ip), nil
		}
	}
	var useProxy = false
	if config.IsZjuForceProxyRuleAvailable() {
		if isInZjuForceProxyRule := config.IsInZjuForceProxyRule(host); isInZjuForceProxyRule {
			useProxy = true
		}
	}
	if !useProxy && config.IsDomainRuleAvailable() {
		if _, found := config.GetSingleDomainRule(host); found {
			useProxy = true
		}
	}

	ctx = context.WithValue(ctx, "USE_PROXY", useProxy)

	if UseZjuDns {
		if cachedIP, found := GetDnsCache(host); found {
			log.Printf("%s -> %s", host, cachedIP.String())
			return ctx, cachedIP, nil
		} else {
			targets, err := resolve.remoteResolver.LookupIP(context.Background(), "ip4", host)
			if err != nil {
				log.Printf("Resolve IPv4 addr failed using ZJU DNS: " + host + ", using local DNS instead.")

				target, err := net.ResolveIPAddr("ip4", host)
				if err != nil {
					log.Printf("Resolve IPv4 addr failed using local DNS: " + host + ". Try IPv6 addr.")

					target, err := net.ResolveIPAddr("ip6", host)
					if err != nil {
						log.Printf("Resolve IPv6 addr failed using local DNS: " + host + ". Reject connection.")
						return ctx, nil, err
					} else {
						log.Printf("%s -> %s", host, target.IP.String())
						return ctx, target.IP, nil
					}
				} else {
					log.Printf("%s -> %s", host, target.IP.String())
					return ctx, target.IP, nil
				}
			} else {
				//TODO: whether we need all dns records? or only 10.0.0.0/8 ?
				SetDnsCache(host, targets[0])
				log.Printf("%s -> %s", host, targets[0].String())
				return ctx, targets[0], nil
			}
		}

	} else {
		// because of OS cache, don't need extra dns memory cache
		target, err := net.ResolveIPAddr("ip4", host)
		if err != nil {
			log.Printf("Resolve IPv4 addr failed using local DNS: " + host + ". Reject connection.")
			return ctx, nil, err
		} else {
			return ctx, target.IP, nil
		}
	}
}

func dialDirect(ctx context.Context, network, addr string) (net.Conn, error) {
	goDialer := &net.Dialer{}
	goDial := goDialer.DialContext

	log.Printf("%s -> DIRECT", addr)

	return goDial(ctx, network, addr)
}

func ServeSocks5(ipStack *stack.Stack, selfIp []byte, bindAddr string) {
	var remoteResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return gonet.DialContextTCP(ctx, ipStack, tcpip.FullAddress{
				NIC:  defaultNIC,
				Port: uint16(53),
				Addr: tcpip.Address(net.ParseIP("10.10.0.21").To4()),
			}, header.IPv4ProtocolNumber)
		},
	}

	var authMethods []socks5.Authenticator
	if SocksUser != "" && SocksPasswd != "" {
		authMethods = append(authMethods, socks5.UserPassAuthenticator{
			Credentials: socks5.StaticCredentials{SocksUser: SocksPasswd},
		})
	} else {
		authMethods = append(authMethods, socks5.NoAuthAuthenticator{})
	}

	var zjuDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {

		// Check if is IPv6
		if strings.Count(addr, ":") > 1 {
			return dialDirect(ctx, network, addr)
		}

		parts := strings.Split(addr, ":")

		// in normal situation, addr must be a pure valid IP
		// because we use `ZJUDnsResolve` to resolve domain name before call `Dial`
		host := parts[0]
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, errors.New("Invalid port in address: " + addr)
		}

		var isInZjuForceProxyRule = false
		var useProxy = false

		var target *net.IPAddr

		if pureIp := net.ParseIP(host); pureIp != nil {
			// host is pure IP format, e.g.: "10.10.10.10"
			target = &net.IPAddr{IP: pureIp}
		} else {
			// illegal situation
			log.Printf("Illegal situation, host is not pure IP format: %s", host)
			return dialDirect(ctx, network, addr)
		}

		if ProxyAll {
			useProxy = true
		}

		if res := ctx.Value("USE_PROXY"); res != nil && res.(bool) {
			useProxy = true
		}

		if !useProxy && config.IsDomainRuleAvailable() {
			_, useProxy = config.GetSingleDomainRule(target.IP.String())
		}

		if !useProxy && config.IsZjuForceProxyRuleAvailable() {
			isInZjuForceProxyRule = config.IsInZjuForceProxyRule(target.IP.String())

			if isInZjuForceProxyRule {
				useProxy = true
			}
		}

		if !useProxy && config.IsIpv4RuleAvailable() {
			if DebugDump {
				log.Printf("IPv4 rule is available ")
			}
			for _, rule := range *config.GetIpv4Rules() {
				if rule.CIDR {
					_, cidr, _ := net.ParseCIDR(rule.Rule)
					if DebugDump {
						log.Printf("CIDR test: %s %s %v", target.IP, rule.Rule, cidr.Contains(target.IP))
					}

					if cidr.Contains(target.IP) {
						if DebugDump {
							log.Printf("CIDR matched: %s %s", target.IP, rule.Rule)
						}

						useProxy = true
					}
				} else {
					if DebugDump {
						log.Printf("Raw match test: %s %s", target.IP, rule.Rule)
					}

					ip1 := net.ParseIP(strings.Split(rule.Rule, "~")[0])
					ip2 := net.ParseIP(strings.Split(rule.Rule, "~")[1])

					if bytes.Compare(target.IP, ip1) >= 0 && bytes.Compare(target.IP, ip2) <= 0 {
						if DebugDump {
							log.Printf("Raw matched: %s %s", ip1, ip2)
						}

						useProxy = true
					}
				}
			}
		}

		if useProxy {
			addrTarget := tcpip.FullAddress{
				NIC:  defaultNIC,
				Port: uint16(port),
				Addr: tcpip.Address(target.IP),
			}

			bind := tcpip.FullAddress{
				NIC:  defaultNIC,
				Addr: tcpip.Address(selfIp),
			}

			if network == "tcp" {
				log.Printf("%s -> PROXY", addr)
				return gonet.DialTCPWithBind(context.Background(), ipStack, bind, addrTarget, header.IPv4ProtocolNumber)
			} else if network == "udp" {
				log.Printf("%s -> PROXY", addr)
				return gonet.DialUDP(ipStack, &bind, &addrTarget, header.IPv4ProtocolNumber)
			} else {
				log.Printf("Proxy only support TCP/UDP. Connection to %s will use direct connection.", addr)
				return dialDirect(ctx, network, addr)
			}
		} else {
			return dialDirect(ctx, network, addr)
		}
	}

	server := socks5.NewServer(
		socks5.WithAuthMethods(authMethods),
		socks5.WithResolver(ZJUDnsResolve{
			remoteResolver: remoteResolver,
		}),
		socks5.WithDial(zjuDialer),
		socks5.WithLogger(socks5.NewLogger(log.New(os.Stdout, "", log.LstdFlags))),
	)

	log.Printf(">>>SOCKS5 SERVER listening on<<<: " + bindAddr)

	if SocksUser != "" && SocksPasswd != "" {
		var Red = "\033[31m"
		var Yellow = "\033[33m"
		var Blue = "\033[34m"
		var Reset = "\033[0m"

		log.Printf(Red + ">>>RFC 1928 所规定的 SOCKS5 只提供流量转发功能，不提供任何加密的手段，数据均为明文传输，安全性极差<<<" + Reset)
		log.Printf(Red + ">>>请勿将其部署至公网提供公开服务，造成的一切后果、责任与开发者无关<<<" + Reset)
		log.Printf(Yellow + ">>>RFC 1928 所规定的 SOCKS5 只提供流量转发功能，不提供任何加密的手段，数据均为明文传输，安全性极差<<<" + Reset)
		log.Printf(Yellow + ">>>请勿将其部署至公网提供公开服务，造成的一切后果、责任与开发者无关<<<" + Reset)
		log.Printf(Blue + ">>>RFC 1928 所规定的 SOCKS5 只提供流量转发功能，不提供任何加密的手段，数据均为明文传输，安全性极差<<<" + Reset)
		log.Printf(Blue + ">>>请勿将其部署至公网提供公开服务，造成的一切后果、责任与开发者无关<<<" + Reset)
	}
	if err := server.ListenAndServe("tcp", bindAddr); err != nil {
		panic("socks listen failed: " + err.Error())
	}
}
