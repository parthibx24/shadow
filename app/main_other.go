// +build !windows

package app

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"

	"github.com/imgk/shadow/device/tun"
	"github.com/imgk/shadow/dns"
	"github.com/imgk/shadow/log"
	"github.com/imgk/shadow/netstack"
	"github.com/imgk/shadow/protocol"
)

func Exit(sigCh chan os.Signal) {
	sigCh <- unix.SIGTERM
}

func Run() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, os.Kill, unix.SIGINT, unix.SIGTERM)

	if err := LoadConfig(file); err != nil {
		log.Logf("load config config.json error: %v", err)

		return
	}
	LoadDomainRules(dns.MatchTree())

	if err := dns.SetResolver(conf.NameServer); err != nil {
		log.Logf("dns server error")
		
		return
	}

	plugin, err := LoadPlugin(conf.Plugin, conf.PluginOpts)
	if conf.Plugin != "" && err != nil {
		log.Logf("plugin %v error: %v", conf.Plugin, err)

		return
	}

	if plugin != nil {
		if plugin.Start(); err != nil {
			log.Logf("plugin start error: %v", err)

			return
		}
		defer plugin.Stop()
		log.Logf("plugin %v start", conf.Plugin)

		go func() {
			if err := plugin.Wait(); err != nil {
				log.Logf("plugin error %v", err)
				Exit(sigCh)

				return
			}
			log.Logf("plugin %v stop", conf.Plugin)
		}()
	}

	handler, err := protocol.NewHandler(conf.Server, time.Minute)
	if err != nil {
		log.Logf("shadowsocks error %v", err)

		return
	}

	dev, err := tun.NewDevice("utun")
	if err != nil {
		log.Logf("tun device error: %v", err)

		return
	}
	defer dev.Close()
	log.Logf("tun device name: %v", dev.Name)

	stack := netstack.NewStack(handler, dev)
	defer stack.Close()
	LoadIPRules(stack.IPFilter)

	go func() {
		if _, err := dev.WriteTo(stack); err != nil {
			log.Logf("netstack exit error: %v", err)
			Exit(sigCh)

			return
		}
	}()

	log.Logf("shadowsocks is running...")
	<-sigCh
	log.Logf("shadowsocks is closing...")
}

func (p *Plugin) Stop() error {
	if err := p.Cmd.Process.Signal(unix.SIGTERM); err != nil {
		if er := p.Cmd.Process.Kill(); er != nil {
			return fmt.Errorf("signal plugin process error: %v, kill plugin process error: %v", err, er)
		}
		p.closed <- struct{}{}

		return fmt.Errorf("signal plugin process error: %v", err)
	}

	select {
	case <-p.closed:
		return nil
	case <-time.After(time.Second):
		if err := p.Cmd.Process.Kill(); err != nil {
			return fmt.Errorf("kill plugin process error: %v", err)
		}
		p.closed <- struct{}{}
	}

	return nil
}