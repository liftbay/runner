// Package api is the runner's client for the Liftbay runner protocol (docs/runner.md).
package api

// Job is everything the runner needs for one build.
type Job struct {
	Build   JobBuild   `json:"build"`
	Project JobProject `json:"project"`
	Env     []EnvVar   `json:"env"`
	Signing Signing    `json:"signing"`
}

type JobBuild struct {
	ID          string `json:"id"`
	Number      int    `json:"number"`
	Platform    string `json:"platform"` // ios | android
	Profile     string `json:"profile"`
	Environment string `json:"environment"`
	GitRef      string `json:"gitRef"`
	// store | internal | simulator
	Distribution string `json:"distribution"`
}

type JobProject struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Framework      string `json:"framework"` // expo | react-native | brownfield
	BundleID       string `json:"bundleId"`
	AndroidPackage string `json:"androidPackage"`
}

type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Sensitive or secret: redacted from logs.
	Secret bool `json:"secret"`
}

type Signing struct {
	Android *AndroidSigning `json:"android"`
	IOS     *IOSSigning     `json:"ios"`
}

type AndroidSigning struct {
	KeystoreBase64 string `json:"keystoreBase64"`
	StorePassword  string `json:"storePassword"`
	KeyAlias       string `json:"keyAlias"`
	KeyPassword    string `json:"keyPassword"`
}

type IOSSigning struct {
	CertificateP12Base64 string `json:"certificateP12Base64"`
	CertificatePassword  string `json:"certificatePassword"`
	// One per bundle id: the app and each extension (widget, notification service, ...).
	ProvisioningProfilesBase64 []string `json:"provisioningProfilesBase64"`
	TeamID                     string   `json:"teamId"`
	ExportMethod               string   `json:"exportMethod"`
}

// Status values the runner reports.
const (
	StatusPreparing = "preparing"
	StatusRunning   = "running"
	StatusUploading = "uploading"
	StatusQueued    = "queued"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

type Phase struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	DurationSec *int   `json:"durationSec"`
}

type Update struct {
	Status      string  `json:"status,omitempty"`
	Phases      []Phase `json:"phases,omitempty"`
	Commit      string  `json:"commit,omitempty"`
	AppVersion  string  `json:"appVersion,omitempty"`
	BuildNumber string  `json:"buildNumber,omitempty"`
}

// Log streams.
const (
	Stdout = "stdout"
	Stderr = "stderr"
	System = "system"
)

type LogLine struct {
	Phase  string `json:"phase,omitempty"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}
