package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"remotemac/internal/native"
	"remotemac/internal/server"
)

var version = "dev"

func main() {
	if os.Getenv("REMOTEMAC_INTERNAL_CURSOR_WATCHDOG") == "1" {
		runtime.LockOSThread()
		if err := native.WatchCursorScale(os.Stdin); err != nil {
			log.Printf("鼠标恢复监护进程：%v", err)
		}
		return
	}

	host := flag.String("host", "", "HTTP 服务监听地址，例如 0.0.0.0:8081")
	showVersion := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("RemoteMAC %s\n", version)
		return
	}

	runtime.LockOSThread()
	if err := native.Initialize(); err != nil {
		log.Fatal(err)
	}

	config, err := server.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	config.Version = version
	if *host != "" {
		config.Addr = *host
	}
	app, err := server.New(config)
	if err != nil {
		log.Fatal(err)
	}
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := app.Shutdown(ctx); err != nil {
				log.Printf("RemoteMAC 关闭时发生错误：%v", err)
			}
			native.StopMainLoop()
		})
	}

	go func() {
		if err := app.Run(); err != nil {
			log.Printf("HTTP 服务已停止：%v", err)
			shutdown()
		}
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		shutdown()
	}()

	native.RunMainLoop()
}
