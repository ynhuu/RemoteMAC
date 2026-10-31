package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"remotemac/internal/netshape"
	"remotemac/internal/session"
)

//go:embed static/*
var staticFiles embed.FS

type App struct {
	config     Config
	router     *gin.Engine
	http       *http.Server
	peers      *session.Manager
	testShaper *netshape.Shaper
}

func New(config Config) (*App, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("访问口令不能为空")
	}
	if strings.TrimSpace(config.Version) == "" {
		config.Version = "dev"
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), requestLog())
	var testShaper *netshape.Shaper
	if config.EnableTestNetworkShaper {
		testShaper = netshape.NewShaper()
	}
	app := &App{
		config: config, router: router, peers: session.NewManager(testShaper), testShaper: testShaper,
	}
	app.routes()
	app.http = &http.Server{
		Addr: config.Addr, Handler: router, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state != http.StateNew {
				return
			}
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetNoDelay(true)
			}
		},
	}
	return app, nil
}

func (a *App) routes() {
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	assetsFS, err := fs.Sub(staticFS, "assets")
	if err != nil {
		panic(err)
	}
	indexHTML, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		panic(err)
	}
	manifest, err := fs.ReadFile(staticFS, "manifest.webmanifest")
	if err != nil {
		panic(err)
	}
	appleTouchIcon, err := fs.ReadFile(staticFS, "apple-touch-icon.png")
	if err != nil {
		panic(err)
	}
	favicon, err := fs.ReadFile(staticFS, "favicon.ico")
	if err != nil {
		panic(err)
	}

	r := a.router
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/manifest.webmanifest", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "application/manifest+json; charset=utf-8", manifest)
	})
	r.GET("/apple-touch-icon.png", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "image/png", appleTouchIcon)
	})
	r.GET("/favicon.ico", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "image/x-icon", favicon)
	})
	r.POST("/api/login", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var body struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || !constantTimeEqual(body.Token, a.config.Token) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "访问口令错误"})
			return
		}
		http.SetCookie(c.Writer, &http.Cookie{
			Name: "remotemac_session", Value: sessionCookieValue(a.config.Token), Path: "/", HttpOnly: true,
			Secure: false, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60,
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	api := r.Group("/api")
	api.Use(a.requireAuth())
	api.GET("/capabilities", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"codec": "H264", "version": a.config.Version,
			"sameHostClient": isSameHostClient(c.Request.RemoteAddr),
		})
	})
	if a.testShaper != nil {
		api.POST("/diagnostics/test-network-rate", a.setTestNetworkRate)
	}
	api.GET("/rtc", a.handleRTC)
	r.StaticFS("/assets", http.FS(assetsFS))
	r.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "接口不存在"})
			return
		}
		if c.Request.URL.Path == "/assets" || strings.HasPrefix(c.Request.URL.Path, "/assets/") {
			c.Status(http.StatusNotFound)
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
}

func isSameHostClient(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	host = strings.SplitN(host, "%", 2)[0]
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return false
	}
	if remoteIP.IsLoopback() {
		return true
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		switch local := address.(type) {
		case *net.IPNet:
			if local.IP.Equal(remoteIP) {
				return true
			}
		case *net.IPAddr:
			if local.IP.Equal(remoteIP) {
				return true
			}
		}
	}
	return false
}

func (a *App) setTestNetworkRate(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var body struct {
		Bitrate int `json:"bitrate"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bitrate 必须是 bit/s 数值"})
		return
	}
	if err := a.testShaper.SetRateBPS(body.Bitrate); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	log.Printf("测试 UDP 出口速率设置为 %d bit/s", body.Bitrate)
	c.JSON(http.StatusOK, gin.H{"bitrate": body.Bitrate, "enabled": body.Bitrate > 0})
}

func (a *App) handleRTC(c *gin.Context) {
	upgrader := websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: sameOriginWebSocket,
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("信令 WebSocket 升级失败：远端=%s，Origin=%q，Host=%q，错误=%v",
			c.Request.RemoteAddr, c.Request.Header.Get("Origin"), c.Request.Host, err)
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close()
	ws := session.NewSignalSocket(conn)
	defer a.peers.CloseSocket(ws)
	for {
		var message session.SignalMessage
		if err := conn.ReadJSON(&message); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("信令 WebSocket 非正常断开：远端=%s，错误=%v", c.Request.RemoteAddr, err)
			}
			return
		}
		if err := a.peers.Handle(ws, message); err != nil {
			retryable := session.IsRetryableError(err)
			fatal := message.Type == "offer" && !retryable || session.IsFatalError(err)
			log.Printf("信令消息处理失败：远端=%s，类型=%s，fatal=%t，retryable=%t，错误=%v",
				c.Request.RemoteAddr, message.Type, fatal, retryable, err)
			userError := err.Error()
			switch message.Type {
			case "resize":
				userError = "分辨率切换失败"
				if fatal {
					userError = "分辨率切换失败，正在重连"
				}
			case "offer":
				switch {
				case strings.Contains(userError, "屏幕录制权限"):
					userError = "请开启屏幕录制权限"
				case strings.Contains(userError, "辅助功能输入权限"):
					userError = "请开启辅助功能权限"
				case strings.Contains(userError, "读取浏览器 H.264 解码能力"):
					userError = "浏览器 H.264 不兼容"
				case strings.Contains(userError, "超出浏览器 H.264 解码能力"):
					userError = "请求画面超出浏览器解码能力"
				case strings.Contains(userError, "已有远程控制会话"):
					userError = "已有设备正在连接"
				case userError != "RemoteMAC 正在关闭":
					userError = "连接桌面失败"
				}
			case "candidate":
				userError = "网络协商失败"
			case "quality":
				userError = "视频设置失败"
			}
			_ = ws.Send(session.SignalMessage{Type: "error", Error: userError, Fatal: fatal, Retryable: retryable})
			if fatal || retryable {
				return
			}
		}
	}
}

func sameOriginWebSocket(request *http.Request) bool {
	return request.TLS == nil && request.Header.Get("Origin") == "http://"+request.Host
}

func (a *App) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie("remotemac_session")
		if err != nil || !constantTimeEqual(cookie, sessionCookieValue(a.config.Token)) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
			return
		}
		c.Next()
	}
}

func (a *App) Run() error {
	log.Printf("RemoteMAC HTTP 已监听 http://%s", a.config.Addr)
	logAccessURLs(a.config.Addr)
	if err := a.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func logAccessURLs(address string) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return
	}
	host = strings.Trim(host, "[]")
	boundIP := net.ParseIP(host)
	if host != "" && (boundIP == nil || !boundIP.IsUnspecified()) {
		log.Printf("RemoteMAC 访问地址：http://%s", net.JoinHostPort(host, port))
		return
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return
	}
	seen := make(map[string]struct{})
	for _, address := range addresses {
		interfaceIP, ok := address.(*net.IPNet)
		if !ok || interfaceIP.IP == nil || interfaceIP.IP.IsLoopback() || interfaceIP.IP.IsLinkLocalUnicast() {
			continue
		}
		urlAddress := net.JoinHostPort(interfaceIP.IP.String(), port)
		if _, ok := seen[urlAddress]; ok {
			continue
		}
		seen[urlAddress] = struct{}{}
		log.Printf("RemoteMAC 局域网访问地址：http://%s", urlAddress)
	}
}

func (a *App) Shutdown(ctx context.Context) error {
	if err := a.peers.CloseAll(); err != nil {
		log.Printf("等待桌面会话回收：%v", err)
	}
	return a.http.Shutdown(ctx)
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func sessionCookieValue(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func requestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		log.Printf("%s %s %s %d %s", c.Request.RemoteAddr, c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(started).Round(time.Millisecond))
	}
}
