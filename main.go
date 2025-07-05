package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// ---------------- CONFIG ----------------

type Config struct {
	Symbol         string `yaml:"symbol"`
	SymbolId       string `yaml:"symbolId"` // Bittap
	ReconnectHours int    `yaml:"reconnectHours"`
	MinHoldSec     int    `yaml:"minHoldSec"`

	NormalThreshold float64 `yaml:"normalThreshold"`
	MaxThreshold    float64 `yaml:"maxThreshold"`
	DiffPrecision   int     `yaml:"diffPrecision"`

	MinProfit   float64 `yaml:"minProfit"` // ROI %
	MaxLoss     float64 `yaml:"maxLoss"`   // ROI %
	HoldMin     int     `yaml:"holdMin"`
	HoldMax     int     `yaml:"holdMax"`
	CooldownSec int     `yaml:"cooldownSec"`

	MinVolume     float64 `yaml:"minVolume"`
	MaxVolume     float64 `yaml:"maxVolume"`
	TradeVolume   float64 `yaml:"tradeVolume"`
	LeverageLevel int     `yaml:"leverageLevel"`

	MaxLogSizeMB int `yaml:"maxLogSizeMB"`

	BittapToken string `yaml:"bittapToken"`
}

func loadConfig(p string) (*Config, error) {
	b, e := ioutil.ReadFile(p)
	if e != nil {
		return nil, e
	}
	var c Config
	return &c, yaml.Unmarshal(b, &c)
}

// --------------- MARKET STATE ----------------

type MarketData struct {
	Kline float64
	Ask1  float64
	Bid1  float64
}

type OrderRecord struct {
	Time         time.Time
	Side         string
	Price        float64
	Volume       float64
	OrderID      string
	PlannedClose string
	ROI          float64
}

var (
	fameexData  = &MarketData{}
	binanceData = &MarketData{}
	dataMu      sync.RWMutex

	posMu      sync.Mutex
	currentPos *OrderRecord
	lastOrderT time.Time

	lastDiff       float64
	closeFailCount int // 平仓失败计数器
)

// --------------- LOGGING ----------------

var (
	rootLog  *log.Logger
	orderLog *log.Logger
)

func initLogger() error {
	_ = os.MkdirAll("logs", os.ModePerm)
	_ = os.MkdirAll("orders", os.ModePerm)
	file := func() string { return filepath.Join("logs", time.Now().Format("20060102_15")+".log") }
	f, e := os.OpenFile(file(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if e != nil {
		return e
	}
	rootLog = log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags)
	of, e := os.OpenFile(filepath.Join("orders", time.Now().Format("20060102")+".csv"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if e != nil {
		return e
	}
	orderLog = log.New(of, "", log.LstdFlags)

	go func() {
		tk := time.NewTicker(time.Hour)
		for range tk.C {
			f.Close()
			if nf, e := os.OpenFile(file(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); e == nil {
				rootLog.SetOutput(io.MultiWriter(os.Stdout, nf))
				f = nf
			}
		}
	}()
	return nil
}

// --------------- MAIN ----------------

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	if err := initLogger(); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()

	go binanceLoop(ctx, cfg)
	go fameexLoop(ctx, cfg)
	tradeLoop(ctx, cfg)
}

// --------------- WEBSOCKET LOOPS ----------------

func binanceLoop(ctx context.Context, cfg *Config) {
	connect := func() (*websocket.Conn, error) {
		s := strings.ToLower(cfg.Symbol)
		url := fmt.Sprintf("wss://fstream.binance.com/stream?streams=%s@kline_1m/%s@depth5@100ms", s, s)
		d := &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		c, _, err := d.Dial(url, nil)
		return c, err
	}
	var c *websocket.Conn
	for {
		if c == nil {
			var err error
			if c, err = connect(); err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			rootLog.Println("Binance 已连接")
		}
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			c.Close()
			c = nil
			continue
		}
		processBinance(msg)
		select {
		case <-ctx.Done():
			c.Close()
			return
		default:
		}
	}
}

func fameexLoop(ctx context.Context, cfg *Config) {
	connect := func() (*websocket.Conn, error) {
		d := &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		c, _, err := d.Dial("wss://futuresws.fameex.com/kline-api/ws", nil)
		if err != nil {
			return nil, err
		}
		s := strings.ToLower(cfg.Symbol)
		for _, sub := range []string{
			fmt.Sprintf(`{"event":"sub","params":{"channel":"market_e_%s_kline_1min","cb_id":"e_%s"}}`, s, s),
			fmt.Sprintf(`{"event":"sub","params":{"channel":"market_e_%s_depth_step1","cb_id":"e_%s"}}`, s, s),
		} {
			_ = c.WriteMessage(websocket.TextMessage, []byte(sub))
		}
		return c, nil
	}
	var c *websocket.Conn
	for {
		if c == nil {
			var err error
			if c, err = connect(); err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			rootLog.Println("FameEX 已连接")
		}
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			c.Close()
			c = nil
			continue
		}
		processFameex(msg)
		select {
		case <-ctx.Done():
			c.Close()
			return
		default:
		}
	}
}

// --------------- MSG PROCESS ----------------

func processBinance(msg []byte) {
	var w struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(msg, &w) != nil {
		return
	}
	dataMu.Lock()
	defer dataMu.Unlock()
	switch {
	case strings.Contains(w.Stream, "@kline"):
		var k struct {
			K struct {
				Close string `json:"c"`
			} `json:"k"`
		}
		if json.Unmarshal(w.Data, &k) == nil {
			binanceData.Kline, _ = strconv.ParseFloat(k.K.Close, 64)
		}
	case strings.Contains(w.Stream, "@depth"):
		var d struct {
			Asks [][]string `json:"a"`
			Bids [][]string `json:"b"`
		}
		if err := json.Unmarshal(w.Data, &d); err == nil {
			if len(d.Asks) > 0 {
				binanceData.Ask1, _ = strconv.ParseFloat(d.Asks[0][0], 64)
			}
			if len(d.Bids) > 0 {
				binanceData.Bid1, _ = strconv.ParseFloat(d.Bids[0][0], 64)
			}
		}
	}
}

func processFameex(raw []byte) {
	data, err := ungzip(raw)
	if err != nil {
		return
	}
	var m struct {
		Channel string          `json:"channel"`
		Tick    json.RawMessage `json:"tick"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	dataMu.Lock()
	defer dataMu.Unlock()
	switch {
	case strings.Contains(m.Channel, "kline"):
		var k struct{ Close float64 }
		if json.Unmarshal(m.Tick, &k) == nil {
			fameexData.Kline = k.Close
		}
	case strings.Contains(m.Channel, "depth"):
		var d struct{ Asks, Buys [][]float64 }
		if json.Unmarshal(m.Tick, &d) == nil {
			if len(d.Asks) > 0 {
				fameexData.Ask1 = d.Asks[0][0]
			}
			if len(d.Buys) > 0 {
				fameexData.Bid1 = d.Buys[0][0]
			}
		}
	}
}

func ungzip(b []byte) ([]byte, error) {
	r, e := gzip.NewReader(bytes.NewBuffer(b))
	if e != nil {
		return nil, e
	}
	defer r.Close()
	return io.ReadAll(r)
}

// --------------- TRADING CORE ----------------

func tradeLoop(ctx context.Context, cfg *Config) {
	tk := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			evaluate(cfg)
		}
	}
}

func evaluate(cfg *Config) {
	dataMu.RLock()
	b, f := *binanceData, *fameexData
	dataMu.RUnlock()
	diff := b.Kline - f.Kline
	if math.Round(diff*100) != math.Round(lastDiff*100) {
		rootLog.Printf("\nBinance K:%.2f A1:%.2f B1:%.2f \n FameEX K:%.2f A1:%.2f B1:%.2f \n Diff:%.2f\n\n",
			b.Kline, b.Ask1, b.Bid1, f.Kline, f.Ask1, f.Bid1, diff)
		lastDiff = diff
	}

	posMu.Lock()
	defer posMu.Unlock()

	if currentPos != nil {
		roi := calcROI(cfg)
		rootLog.Printf("当前 ROI: %.2f%%", roi)
		if time.Since(currentPos.Time) < time.Duration(cfg.MinHoldSec)*time.Second {
			return
		}

		if roi >= cfg.MinProfit || roi <= cfg.MaxLoss || time.Now().After(parseTime(currentPos.PlannedClose)) {
			if err := closeAll(cfg, roi); err != nil {
				rootLog.Println("平仓失败:", err)
			}
		}
		return
	}

	if time.Since(lastOrderT) < time.Duration(cfg.CooldownSec)*time.Second {
		return
	}
	if math.Abs(diff) < cfg.NormalThreshold || math.Abs(diff) > cfg.MaxThreshold {
		return
	}

	side := "BUY"
	if diff < 0 {
		side = "SELL"
	}

	// ① 生成随机下单数量（0.001–0.150，保留 3 位）
	v := cfg.MinVolume + rand.Float64()*(cfg.MaxVolume-cfg.MinVolume)
	v = math.Trunc(v*1000) / 1000 // 精度 0.001

	oid, px, err := placeOrder(cfg, side, v)
	if err != nil {
		rootLog.Println(err)
		return
	}

	hold := time.Duration(cfg.HoldMin+rand.Intn(cfg.HoldMax-cfg.HoldMin+1)) * time.Minute
	currentPos = &OrderRecord{
		//Time: time.Now(), Side: side, Price: px, Volume: cfg.TradeVolume,
		Time: time.Now(), Side: side, Price: px, Volume: v,
		OrderID: oid, PlannedClose: time.Now().Add(hold).Format(time.RFC3339),
	}
	orderLog.Printf("OPEN,%s,%s,%.2f,%.4f,%s,%s,0.00%%\n",
		//currentPos.Time.Format(time.RFC3339), side, px, cfg.TradeVolume, oid, currentPos.PlannedClose)
		currentPos.Time.Format(time.RFC3339), side, px, v, oid, currentPos.PlannedClose)
	lastOrderT = time.Now()
}

func calcROI(cfg *Config) float64 {
	if currentPos == nil {
		return 0
	}
	dataMu.RLock()
	defer dataMu.RUnlock()
	mkt := fameexData.Bid1
	if currentPos.Side == "SELL" {
		mkt = fameexData.Ask1
	}
	pnl := (mkt - currentPos.Price) / currentPos.Price * 100
	if currentPos.Side == "SELL" {
		pnl = -pnl
	}
	return pnl * float64(cfg.LeverageLevel)
}

// --------------- HTTP API ----------------

// 旧：func placeOrder(cfg *Config, side string) (string, float64, error)
func placeOrder(cfg *Config, side string, volume float64) (string, float64, error) {
	url := "https://api.bittap.com/futures/api/v1/order"
	body := map[string]interface{}{
		"symbolId":    cfg.SymbolId,
		"side":        side,
		"quantity":    volume,
		"type":        "MARKET",
		"postOnly":    false,
		"timeInForce": "GTC",
		"reduceOnly":  false,
		"tpSl": map[string]interface{}{
			"tpPrice": "0",
			"tpType":  "MARK",
			"slPrice": "0",
			"slType":  "MARK",
		},
	}

	rawBody, _ := json.Marshal(body)
	rootLog.Printf("请求 Body: %s", string(rawBody))
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(rawBody))
	setHdr(req, cfg)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	resRaw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data string `json:"data"`
	}
	if json.Unmarshal(resRaw, &res) != nil || res.Code != "0" {
		rootLog.Printf("下单失败原文: %s", resRaw)
		return "", 0, fmt.Errorf("下单失败 code=%s msg=%s", res.Code, res.Msg)
	}

	time.Sleep(1000 * time.Millisecond) // 防止立即查询持仓太快

	// 查询持仓，获取真实开仓均价
	openPrice, err := queryOpenAvgPrice(cfg)
	if err != nil {
		rootLog.Printf("查询开仓均价失败: %v", err)
		// 如果失败则 fallback 使用盘口价格
		openPrice = fameexData.Ask1
		if side == "SELL" {
			openPrice = fameexData.Bid1
		}
	}
	return res.Data, openPrice, nil
}

func queryOpenAvgPrice(cfg *Config) (float64, error) {
	url := "https://api.bittap.com/futures/api/v1/position/open?showAll=true&pageNumber=1&pageSize=200"
	req, _ := http.NewRequest("GET", url, strings.NewReader(""))
	setHdr(req, cfg)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	resRaw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code string `json:"code"`
		Data struct {
			Records []struct {
				SymbolId      string `json:"symbolId"`
				AvgEntryPrice string `json:"avgEntryPrice"`
				Quantity      string `json:"quantity"` // ✅ 改为 string
			} `json:"records"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resRaw, &res); err != nil {
		return 0, err
	}
	if res.Code != "0" {
		return 0, fmt.Errorf("查询失败 code=%s", res.Code)
	}
	for _, p := range res.Data.Records {
		qty, err := strconv.ParseFloat(p.Quantity, 64)
		if err != nil {
			return 0, fmt.Errorf("解析 quantity 失败: %v", err)
		}
		if p.SymbolId == cfg.SymbolId && qty > 0 {
			price, err := strconv.ParseFloat(p.AvgEntryPrice, 64)
			if err != nil {
				return 0, fmt.Errorf("解析 avgEntryPrice 失败: %v", err)
			}
			rootLog.Printf("查询到的持仓价格: %.4f", price)
			return price, nil
		}
	}
	return 0, fmt.Errorf("未找到对应持仓")
}

func closeAll(cfg *Config, roi float64) error {
	url := "https://api.bittap.com/futures/api/v1/position/close-all"
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	setHdr(req, cfg)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		handleCloseFail(err)
		return err
	}
	defer resp.Body.Close()

	resRaw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(resRaw, &res) != nil || res.Code != "0" {
		rootLog.Printf("平仓失败原文: %s", resRaw)
		handleCloseFail(fmt.Errorf("平仓异常 code=%s", res.Code))
		return fmt.Errorf("平仓异常 code=%s", res.Code)
	}

	orderLog.Printf("CLOSE,%s,%s,%.2f,%.4f,%s,-,%.2f%%\n",
		time.Now().Format(time.RFC3339), currentPos.Side, fameexData.Kline,
		currentPos.Volume, currentPos.OrderID, roi)

	currentPos = nil
	lastOrderT = time.Now()
	closeFailCount = 0 // ✅ 成功清零计数器
	time.Sleep(500 * time.Millisecond)
	return nil
}

func handleCloseFail(err error) {
	closeFailCount++
	rootLog.Printf("平仓失败（第 %d 次）：%v", closeFailCount, err)
	if closeFailCount >= 10 {
		rootLog.Println("连续平仓失败超过 10 次，程序即将退出！")
		os.Exit(1)
	}
}

func setHdr(r *http.Request, cfg *Config) {
	h := r.Header
	h.Set("Content-Type", "application/json")
	h.Set("authorization", cfg.BittapToken)
	h.Set("accept-language", "zh-CN")
	h.Set("client-type", "pc")
	h.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
}

func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
