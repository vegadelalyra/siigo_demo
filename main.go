package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// --- Configuration & Constants ---

const (
	SandboxURL    = "https://services.siigo.com/alliances/api"
	ProductionURL = "https://api.siigo.com/v1"
)

type Config struct {
	Username    string
	AccessKey   string
	PartnerID   string
	Environment string // "sandbox" or "production"
}

// --- Models ---

type AuthResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type SiigoToken struct {
	Token     string
	ExpiresAt time.Time
	Mu        sync.RWMutex
}

type CustomerRequest struct {
	PersonType             string   `json:"person_type"` // "Person" or "Company"
	IDType                 string   `json:"id_type"`     // e.g., "13"
	Identification         string   `json:"identification"`
	CheckDigit             string   `json:"check_digit,omitempty"`
	Name                   []string `json:"name"`
	FiscalResponsibilities []string `json:"fiscal_responsibilities"`
	Address                Address  `json:"address"`
}

type Address struct {
	Address string `json:"address"`
	City    City   `json:"city"`
}

type City struct {
	CountryCode string `json:"country_code"`
	StateCode   string `json:"state_code"`
	CityCode    string `json:"city_code"`
}

type ProductRequest struct {
	Code         string   `json:"code"`
	Name         string   `json:"name"`
	AccountGroup int      `json:"account_group"`
	Type         string   `json:"type"`
	StockControl bool     `json:"stock_control"`
	Taxed        bool     `json:"taxed"`
	Prices       []Price  `json:"prices"`
}

type Price struct {
	CurrencyCode string  `json:"currency_code"`
	Value        float64 `json:"value"`
}

type POSBillRequest struct {
	DocumentID int           `json:"document_id"`
	Date       string        `json:"date"` // YYYY-MM-DD
	Customer   Identification `json:"customer"`
	Items      []BillItem    `json:"items"`
	Payments   []Payment     `json:"payments"`
}

type Identification struct {
	Identification string `json:"identification"`
}

type BillItem struct {
	Code     string  `json:"code"`
	Quantity float64 `json:"quantity"`
	Price    float64 `json:"price"`
}

type Payment struct {
	ID    int     `json:"id"`
	Value float64 `json:"value"`
}

// --- Siigo Client Logic ---

type SiigoClient struct {
	Cfg   Config
	Token *SiigoToken
	HTTP  *http.Client
}

func NewSiigoClient(cfg Config) *SiigoClient {
	return &SiigoClient{
		Cfg:   cfg,
		Token: &SiigoToken{},
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *SiigoClient) GetBaseURL() string {
	if c.Cfg.Environment == "production" {
		return ProductionURL
	}
	return SandboxURL
}

// Authenticate exchanges credentials for a Bearer token
func (c *SiigoClient) Authenticate() error {
	c.Token.Mu.Lock()
	defer c.Token.Mu.Unlock()

	// Check if existing token is still valid (with 1-minute buffer)
	if time.Now().Add(1 * time.Minute).Before(c.Token.ExpiresAt) {
		return nil
	}

	payload := map[string]string{
		"username":   c.Cfg.Username,
		"access_key": c.Cfg.AccessKey,
	}
	body, _ := json.Marshal(payload)

	authURL := fmt.Sprintf("%s/auth", c.GetBaseURL())
	req, _ := http.NewRequest("POST", authURL, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Partner-Id", c.Cfg.PartnerID)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authentication failed: %d", resp.StatusCode)
	}

	var authResp AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return err
	}

	c.Token.Token = authResp.AccessToken
	c.Token.ExpiresAt = time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second)
	
	log.Printf("Successfully authenticated with Siigo. Token expires in %d seconds", authResp.ExpiresIn)
	return nil
}

// DoRequest wraps HTTP calls with Auth and Partner headers
func (c *SiigoClient) DoRequest(method, endpoint string, body interface{}) (*http.Response, error) {
	if err := c.Authenticate(); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if body != nil {
		jsonData, _ := json.Marshal(body)
		bodyReader = bytes.NewBuffer(jsonData)
	}

	url := fmt.Sprintf("%s%s", c.GetBaseURL(), endpoint)
	req, _ := http.NewRequest(method, url, bodyReader)
	
	c.Token.Mu.RLock()
	req.Header.Set("Authorization", "Bearer "+c.Token.Token)
	c.Token.Mu.RUnlock()
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Partner-Id", c.Cfg.PartnerID)

	return c.HTTP.Do(req)
}

// --- API Handlers (Proxy) ---

func main() {
	// Initialize Config from Env (Fallback to Sandbox defaults)
	cfg := Config{
		Username:    getEnv("SIIGO_USER", "siigoapi@pruebas.com"),
		AccessKey:   getEnv("SIIGO_KEY", "YOUR_SANDBOX_KEY"),
		PartnerID:   getEnv("SIIGO_PARTNER", "MyAppName"),
		Environment: getEnv("SIIGO_ENV", "sandbox"),
	}

	client := NewSiigoClient(cfg)
	r := gin.Default()

	// 1. Create Customer
	r.POST("/v1/customers", func(c *gin.Context) {
		var req CustomerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "Invalid request payload"})
			return
		}

		resp, err := client.DoRequest("POST", "/customers", req)
		handleSiigoResponse(c, resp, err)
	})

	// 2. Create Product
	r.POST("/v1/products", func(c *gin.Context) {
		var req ProductRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "Invalid request payload"})
			return
		}

		resp, err := client.DoRequest("POST", "/products", req)
		handleSiigoResponse(c, resp, err)
	})

	// 3. Create POS Bill (Invoice)
	r.POST("/v1/pos-bills", func(c *gin.Context) {
		var req POSBillRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "Invalid request payload"})
			return
		}

		// In Siigo, invoices and POS are usually the same endpoint, 
		// distinguished by the 'DocumentID'
		resp, err := client.DoRequest("POST", "/invoices", req)
		handleSiigoResponse(c, resp, err)
	})

	// Health Check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "alive", "env": cfg.Environment})
	})

	port := getEnv("PORT", "8080")
	fmt.Printf("Siigo Proxy API running on :%s in %s mode\n", port, cfg.Environment)
	r.Run(":" + port)
}

// --- Utilities ---

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func handleSiigoResponse(c *gin.Context, resp *http.Response, err error) {
	if err != nil {
		c.JSON(502, gin.H{"error": "Failed to communicate with Siigo", "details": err.Error()})
		return
	}
	defer resp.Body.Close()

	var result interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	
	// Forward the exact status code and body from Siigo to the Postman client
	c.JSON(resp.StatusCode, result)
}
