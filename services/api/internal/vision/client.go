package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type FaceResult struct {
	Box        []int   `json:"box"`
	DetScore   float64 `json:"det_score"`
	Matched    bool    `json:"matched"`
	Name       string  `json:"name"`
	Similarity float64 `json:"similarity"`
	Distance   float64 `json:"distance"`
}

type PoseResult struct {
	Box         []int     `json:"box"`
	Score       float64   `json:"score"`
	IsFall      bool      `json:"is_fall"`
	FallScore   float64   `json:"fall_score"`
	Pose        string    `json:"pose"`
	AspectRatio float64   `json:"aspect_ratio"`
	TorsoAngle  float64   `json:"torso_angle"`
	Keypoints   [][]any   `json:"keypoints"`
}

type AnalyzeRequest struct {
	ImageURL    string `json:"image_url,omitempty"`
	ImageBase64 string `json:"image,omitempty"`
	DetectFace  bool   `json:"detect_face"`
	DetectPose  bool   `json:"detect_pose"`
}

type AnalyzeResponse struct {
	Code int `json:"code"`
	Data struct {
		Faces          []FaceResult `json:"faces"`
		Poses          []PoseResult `json:"poses"`
		HasFall        bool         `json:"has_fall"`
		MatchedPersons []string     `json:"matched_persons"`
	} `json:"data"`
	Message string `json:"message,omitempty"`
}

type HealthResponse struct {
	Status            string `json:"status"`
	FaceEngineReady   bool   `json:"face_engine_ready"`
	PoseEngineReady   bool   `json:"pose_engine_ready"`
	RegisteredPersons int    `json:"registered_persons"`
}

type PersonItem struct {
	Name string `json:"name"`
}

type PersonsListResponse struct {
	Code int          `json:"code"`
	Data []PersonItem `json:"data"`
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://home-vision:8090"
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vision health returned status %d", resp.StatusCode)
	}

	var hr HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return nil, err
	}
	return &hr, nil
}

func (c *Client) Analyze(ctx context.Context, ar AnalyzeRequest) (*AnalyzeResponse, error) {
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/analyze", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vision analyze error %d: %s", resp.StatusCode, string(b))
	}

	var res AnalyzeResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ListPersons(ctx context.Context) ([]PersonItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/face/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var plr PersonsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&plr); err != nil {
		return nil, err
	}
	return plr.Data, nil
}

func (c *Client) RegisterPerson(ctx context.Context, name string, imageBase64 string) error {
	payload := map[string]string{
		"name":  name,
		"image": imageBase64,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/face/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register person failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) DeletePerson(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/v1/face/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete person failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}
