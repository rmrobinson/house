package main

// defaultPort is Nanoleaf's standard control port for its Light Panels HTTP API.
const defaultPort = 16021

// deviceConfig describes a single configured Nanoleaf Light Panels controller. Each entry gets
// its own persistent connection - see panel.go.
type deviceConfig struct {
	ID   string `mapstructure:"id"`
	Name string `mapstructure:"name"`

	Host string `mapstructure:"host"`
	// Port defaults to defaultPort if unset.
	Port int `mapstructure:"port"`

	// APIKey is obtained via cmd/pair (press the panel's pairing button, then run it) and
	// pasted in here - mirrors bridges/esphome's noise_psk: a single opaque secret string, no
	// separate pairing store needed.
	APIKey string `mapstructure:"api_key"`
}
