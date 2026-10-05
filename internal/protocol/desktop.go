package protocol

// Desktop uses a JSON result followed by exactly one JPEG FrameData when
// ImageBytes is nonzero. The old exec screenshot stream remains raw PNG.
const (
	KindDesktop        = "desktop"
	KindDesktopResult  = "desktop_result"
	DesktopWindowsOnly = "desktop: Windows only"
	DesktopHumanInput  = "有人在用这台电脑"
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}
type DesktopMonitor struct {
	ID      string `json:"id"`
	Rect    Rect   `json:"rect"` // physical pixels, in virtual desktop coordinates
	Primary bool   `json:"primary"`
	DPI     int    `json:"dpi"`
}
type DesktopWindow struct {
	ID             string `json:"id"`
	PID            uint32 `json:"pid"`
	Title          string `json:"title"`
	Process        string `json:"process"`
	Rect           Rect   `json:"rect"` // physical pixels
	Elevated       bool   `json:"elevated"`
	ElevationKnown bool   `json:"elevation_known"`
}
type DesktopSnapshot struct {
	ID         string           `json:"id"`
	CapturedAt int64            `json:"captured_at"` // Unix milliseconds
	Session    uint32           `json:"session"`
	Layout     string           `json:"layout"` // opaque display/session signature
	Origin     Point            `json:"origin"`
	Source     Rect             `json:"source"` // physical source rectangle of this image
	Width      int              `json:"width"`
	Height     int              `json:"height"`
	Scale      float64          `json:"scale"` // image width / physical source width; use dimensions for rounding
	Monitors   []DesktopMonitor `json:"monitors"`
	Foreground DesktopWindow    `json:"foreground"`
	Windows    []DesktopWindow  `json:"windows"` // front to back, visible top-level windows
	Locked     bool             `json:"locked"`
}
type DesktopAction struct {
	Type      string   `json:"type"`
	X         int      `json:"x,omitempty"`
	Y         int      `json:"y,omitempty"`
	ToX       int      `json:"to_x,omitempty"`
	ToY       int      `json:"to_y,omitempty"`
	Button    string   `json:"button,omitempty"`
	Count     int      `json:"count,omitempty"`
	Delta     int      `json:"delta,omitempty"` // wheel notches; positive scrolls up
	Text      string   `json:"text,omitempty"`
	Key       string   `json:"key,omitempty"`
	Millis    int      `json:"ms,omitempty"`
	Title     string   `json:"title,omitempty"`
	PID       uint32   `json:"pid,omitempty"`
	Program   string   `json:"program,omitempty"`
	Args      []string `json:"args,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}
type DesktopRequest struct {
	ScreenshotID string          `json:"screenshot_id,omitempty"`
	Region       *Rect           `json:"region,omitempty"` // crop expressed in the referenced full screenshot's pixels
	Actions      []DesktopAction `json:"actions,omitempty"`
}
type DesktopActionResult struct {
	Index          int    `json:"index"`
	Type           string `json:"type"`
	Status         string `json:"status"` // input_sent, completed, partial, rejected
	Error          string `json:"error,omitempty"`
	PID            uint32 `json:"pid,omitempty"`
	WindowAppeared bool   `json:"window_appeared,omitempty"`
	Foreground     bool   `json:"foreground,omitempty"`
}
type DesktopResult struct {
	RequestID   string                `json:"request_id,omitempty"`
	Status      string                `json:"status"` // completed, rejected, partial, interrupted, unknown
	Error       string                `json:"error,omitempty"`
	Warning     string                `json:"warning,omitempty"` // cleanup uncertainty; never suppresses the human-input stop result
	Completed   int                   `json:"completed"`
	FailedIndex int                   `json:"failed_index"` // -1 when no action failed
	Actions     []DesktopActionResult `json:"actions,omitempty"`
	Snapshot    *DesktopSnapshot      `json:"snapshot,omitempty"`
	ImageBytes  int                   `json:"image_bytes"`
	MIME        string                `json:"mime,omitempty"`
}
