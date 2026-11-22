package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Jamir-boop/stt-router/internal/config"
)

const unit = "stt-router.service"

const usage = `Usage: stt-router [-config FILE] [-listen ADDR] [COMMAND]
  serve       Run the router (the default with no command).
  dashboard   Open the dashboard and copy the management key to the clipboard.
  status | start | stop | restart
              Control the systemd user service (Linux).
  logs        Follow the service log (Linux).
  version     Print the version.
  help        Show this text.
`

// cli runs a helper command. It never prints a key.
func cli(cmd, configPath string) error {
	switch cmd {
	case "dashboard":
		return openDashboard(configPath)
	case "status", "start", "stop", "restart":
		return passthrough("systemctl", "--user", cmd, unit, "--no-pager")
	case "logs":
		return passthrough("journalctl", "--user", "-u", unit, "-n", "50", "-f")
	case "version":
		fmt.Println("stt-router", version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q; run: stt-router help", cmd)
}

func passthrough(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func openDashboard(configPath string) error {
	cfg, _, err := config.Load(configPath)
	if err != nil {
		return err
	}
	url := dashboardURL(cfg.Listen)
	key := config.Resolve(cfg.ManagementKey)
	copied := key != "" && copyToClipboard(key)
	if err := openBrowser(url); err != nil {
		fmt.Printf("Could not open a browser. Open %s yourself.\n", url)
	} else {
		fmt.Printf("Dashboard opened: %s\n", url)
	}
	switch {
	case copied:
		fmt.Println("The management key is in the clipboard. Paste it into the login field.")
	case key == "":
		fmt.Println("No management key is set yet. Start the router once to generate it.")
	default:
		fmt.Printf("No clipboard tool found (wl-copy, xclip, xsel). The key is the management_key value in %s.\n", configPath)
	}
	return nil
}

// dashboardURL turns a listen address into a URL a local browser can open.
func dashboardURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func copyToClipboard(text string) bool {
	var tools [][]string
	switch runtime.GOOS {
	case "darwin":
		tools = [][]string{{"pbcopy"}}
	case "windows":
		tools = [][]string{{"clip"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			tools = append(tools, []string{"wl-copy"})
		}
		tools = append(tools, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
	}
	for _, t := range tools {
		c := exec.Command(t[0], t[1:]...)
		c.Stdin = strings.NewReader(text)
		if c.Run() == nil {
			return true
		}
	}
	return false
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return errors.New("xdg-open not found")
	}
	return exec.Command("xdg-open", url).Start()
}
