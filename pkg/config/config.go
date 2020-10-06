package config

import (
	"fmt"
	"io/ioutil"
	"path/filepath"

	"github.com/hashicorp/go-multierror"
	"github.com/naoina/toml"
	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/model"
)

// Options for each of sponsorblock's categories. Each should be one of "cut", "keep", or "default" if in a feed.
// Has no effect if `sponsorblock_mode` is `off`
type SponsorBlockCategories struct {
	// Sponsor category: Paid promotion, paid referrals and direct advertisements. Not for self-promotion or free shoutouts to causes/creators/websites/products they like.
	Sponsors string `toml:"sponsors"`
	// Intermission/Intro Animation category: An interval without actual content. Could be a pause, static frame, repeating animation. This should not be used for transitions containing information or be used on music videos.
	Intermissions string `toml:"intermissions`
	// Endcards/Credits category: Credits or when the YouTube endcards appear. Not for spoken conclusions. This should not include useful content. This should not be used on music videos.
	Endcards string `toml:"endcards"`
	// Interaction Reminder (Subscribe) category: When there is a short reminder to like, subscribe or follow them in the middle of content. If it is long or about something specific, it should be under self promotion instead.
	InteractionReminders string `toml:"interaction_reminders"`
	// Unpaid/Self Promotion category: Similar to "sponsor" except for unpaid or self promotion. This includes sections about merchandise, donations, or information about who they collaborated with.
	SelfPromotions string `toml:"self_promotions"`
	// Non-Music Section category: Only for use in music videos. This includes introductions or outros in music videos.
	NonmusicSections string `toml:"nonmusic_sections"`
}

// Feed is a configuration for a feed
type Feed struct {
	ID string `toml:"-"`
	// URL is a full URL of the field
	URL string `toml:"url"`
	// PageSize is the number of pages to query from YouTube API.
	// NOTE: larger page sizes/often requests might drain your API token.
	PageSize int `toml:"page_size"`
	// UpdatePeriod is how often to check for updates.
	// Format is "300ms", "1.5h" or "2h45m".
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	// NOTE: too often update check might drain your API token.
	UpdatePeriod Duration `toml:"update_period"`
	// Cron expression format is how often to check update
	// NOTE: too often update check might drain your API token.
	CronSchedule string `toml:"cron_schedule"`
	// Quality to use for this feed
	Quality model.Quality `toml:"quality"`
	// Maximum height of video
	MaxHeight int `toml:"max_height"`
	// Format to use for this feed
	Format model.Format `toml:"format"`
	// Only download episodes that match this regexp (defaults to matching anything)
	Filters Filters `toml:"filters"`
	// Clean is a cleanup policy to use for this feed
	Clean Cleanup `toml:"clean"`
	// Custom is a list of feed customizations
	Custom Custom `toml:"custom"`
	// List of additional youtube-dl arguments passed at download time
	YouTubeDLArgs []string `toml:"youtube_dl_args"`
	// Included in OPML file
	OPML bool `toml:"opml"`
	// Whether to cut out sponsor segments using sponsorblock.
	// One of:
	// "default"      - Use the mode from global config
	// "off"          - Don't use sponsorblock for this feed
	// "require"      - Require segments to be submitted before adding an episode to this feed
	// "delay"        - Wait for the duration specified in `sponsorblock_delay` to add an episode to this feed
	// "requiredelay" - Wait for either segments to be submitted OR the duration in `sponsorblock_delay`
	SponsorblockMode string `toml:"sponsorblock_mode"`
	// How long to wait, if `sponsorblock_mode` is "delay" or "requiredelay"
	SponsorblockDelay Duration `toml:"sponsorblock_delay"`
	// What to do with each category of segments from sponsorblock
	SponsorBlockCategories SponsorBlockCategories `toml:"sponsorblock_categories`
}

func IsValidSponsorblockMode(mode string, inFeed bool) bool {
	switch mode {
	case
		"off",
		"require",
		"delay",
		"requiredelay":
		return true
	case
		"default":
		return inFeed
	}
	return false
}

func IsValidCategoryMode(mode string, inFeed bool) bool {
	switch mode {
	case
		"cut",
		"keep":
		return true
	case
		"default":
		return inFeed
	}
	return false
}

type Filters struct {
	Title          string `toml:"title"`
	NotTitle       string `toml:"not_title"`
	Description    string `toml:"description"`
	NotDescription string `toml:"not_description"`
	// More filters to be added here
}

type Custom struct {
	CoverArt string `toml:"cover_art"`
	Category string `toml:"category"`
	Explicit bool   `toml:"explicit"`
	Language string `toml:"lang"`
}

type Server struct {
	// Hostname to use for download links
	Hostname string `toml:"hostname"`
	// Port is a server port to listen to
	Port int `toml:"port"`
	// DataDir is a path to a directory to keep XML feeds and downloaded episodes,
	// that will be available to user via web server for download.
	DataDir string `toml:"data_dir"`
}

type Database struct {
	// Dir is a directory to keep database files
	Dir    string  `toml:"dir"`
	Badger *Badger `toml:"badger"`
}

// Badger represents BadgerDB configuration parameters
// See https://github.com/dgraph-io/badger#memory-usage
type Badger struct {
	Truncate bool `toml:"truncate"`
	FileIO   bool `toml:"file_io"`
}

type Cleanup struct {
	// KeepLast defines how many episodes to keep
	KeepLast int `toml:"keep_last"`
}

type Log struct {
	// Filename to write the log to (instead of stdout)
	Filename string `toml:"filename"`
	// MaxSize is the maximum size of the log file in MB
	MaxSize int `toml:"max_size"`
	// MaxBackups is the maximum number of log file backups to keep after rotation
	MaxBackups int `toml:"max_backups"`
	// MaxAge is the maximum number of days to keep the logs for
	MaxAge int `toml:"max_age"`
	// Compress old backups
	Compress bool `toml:"compress"`
}

// Downloader is a youtube-dl related configuration
type Downloader struct {
	// SelfUpdate toggles self update every 24 hour
	SelfUpdate bool `toml:"self_update"`
}

type SponsorBlock struct {
	// Base URL for sponsorblock api; Should be "https://sponsor.ajay.app" unless a custom server is being used
	ApiUrl string `toml:"url"`
	// Default mode for sponsorblock
	DefaultMode string `toml:"default_mode"`
	// Default amount of time to wait if effective mode is "delay" or "requiredelay"
	DefaultDelay Duration `toml:"default_delay"`
	// What to do by default with each category of segments from sponsorblock
	SponsorBlockCategories SponsorBlockCategories `toml:"sponsorblock_categories`
}

type Config struct {
	// Server is the web server configuration
	Server Server `toml:"server"`
	// Log is the optional logging configuration
	Log Log `toml:"log"`
	// Database configuration
	Database Database `toml:"database"`
	// Feeds is a list of feeds to host by this app.
	// ID will be used as feed ID in http://podsync.net/{FEED_ID}.xml
	Feeds map[string]*Feed
	// Tokens is API keys to use to access YouTube/Vimeo APIs.
	Tokens map[model.Provider]StringSlice `toml:"tokens"`
	// Downloader (youtube-dl) configuration
	Downloader Downloader `toml:"downloader"`
	// Global SponsorBlock config
	SponsorBlock SponsorBlock `toml:"sponsorblock"`
}

// LoadConfig loads TOML configuration from a file path
func LoadConfig(path string) (*Config, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read config file: %s", path)
	}

	config := Config{}
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal toml")
	}

	for id, feed := range config.Feeds {
		feed.ID = id
	}

	config.applyDefaults(path)

	if err := config.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func (c *Config) validate() error {
	var result *multierror.Error

	if c.Server.DataDir == "" {
		result = multierror.Append(result, errors.New("data directory is required"))
	}

	if len(c.Feeds) == 0 {
		result = multierror.Append(result, errors.New("at least one feed must be speficied"))
	}

	if !IsValidSponsorblockMode(c.SponsorBlock.DefaultMode, false) {
		result = multierror.Append(result, errors.Errorf("invalid sponsorblock.default_mode %q", c.SponsorBlock.DefaultMode))
	}

	//TODO: Check SponsorblockCategories for validity

	for id, feed := range c.Feeds {
		if feed.URL == "" {
			result = multierror.Append(result, errors.Errorf("URL is required for %q", id))
		}

		if !IsValidSponsorblockMode(feed.SponsorblockMode, true) {
			result = multierror.Append(result, errors.Errorf("Invalid sponsorblock_mode %q for feed %q", feed.SponsorblockMode, id))
		}
	}

	return result.ErrorOrNil()
}

func (c *Config) applyDefaults(configPath string) {
	if c.Server.Hostname == "" {
		if c.Server.Port != 0 && c.Server.Port != 80 {
			c.Server.Hostname = fmt.Sprintf("http://localhost:%d", c.Server.Port)
		} else {
			c.Server.Hostname = "http://localhost"
		}
	}

	if c.Log.Filename != "" {
		if c.Log.MaxSize == 0 {
			c.Log.MaxSize = model.DefaultLogMaxSize
		}
		if c.Log.MaxAge == 0 {
			c.Log.MaxAge = model.DefaultLogMaxAge
		}
		if c.Log.MaxBackups == 0 {
			c.Log.MaxBackups = model.DefaultLogMaxBackups
		}
	}

	if c.Database.Dir == "" {
		c.Database.Dir = filepath.Join(filepath.Dir(configPath), "db")
	}

	if c.SponsorBlock.ApiUrl == "" {
		c.SponsorBlock.ApiUrl = "https://sponsor.ajay.app"
	}

	if c.SponsorBlock.DefaultMode == "" {
		c.SponsorBlock.DefaultMode = "off"
	}

	// These category defaults should match sponsorblock's defaults
	if c.SponsorBlock.SponsorBlockCategories.Sponsors == "" {
		c.SponsorBlock.SponsorBlockCategories.Sponsors = "cut"
	}

	if c.SponsorBlock.SponsorBlockCategories.Intermissions == "" {
		c.SponsorBlock.SponsorBlockCategories.Intermissions = "keep"
	}

	if c.SponsorBlock.SponsorBlockCategories.Endcards == "" {
		c.SponsorBlock.SponsorBlockCategories.Endcards = "keep"
	}

	if c.SponsorBlock.SponsorBlockCategories.InteractionReminders == "" {
		c.SponsorBlock.SponsorBlockCategories.InteractionReminders = "keep"
	}

	if c.SponsorBlock.SponsorBlockCategories.SelfPromotions == "" {
		c.SponsorBlock.SponsorBlockCategories.SelfPromotions = "keep"
	}

	if c.SponsorBlock.SponsorBlockCategories.NonmusicSections == "" {
		c.SponsorBlock.SponsorBlockCategories.NonmusicSections = "cut"
	}

	for _, feed := range c.Feeds {
		if feed.UpdatePeriod.Duration == 0 {
			feed.UpdatePeriod.Duration = model.DefaultUpdatePeriod
		}

		if feed.Quality == "" {
			feed.Quality = model.DefaultQuality
		}

		if feed.Format == "" {
			feed.Format = model.DefaultFormat
		}

		if feed.PageSize == 0 {
			feed.PageSize = model.DefaultPageSize
		}

		zeroDuration := Duration{}
		if feed.SponsorblockDelay == zeroDuration {
			feed.SponsorblockDelay = c.SponsorBlock.DefaultDelay
		}

		if feed.SponsorblockMode == "" || feed.SponsorblockMode == "default" {
			feed.SponsorblockMode = c.SponsorBlock.DefaultMode
		}

		if feed.SponsorBlockCategories.Sponsors == "" || feed.SponsorBlockCategories.Sponsors == "default" {
			feed.SponsorBlockCategories.Sponsors = c.SponsorBlock.SponsorBlockCategories.Sponsors
		}

		if feed.SponsorBlockCategories.Intermissions == "" || feed.SponsorBlockCategories.Intermissions == "default" {
			feed.SponsorBlockCategories.Intermissions = c.SponsorBlock.SponsorBlockCategories.Intermissions
		}

		if feed.SponsorBlockCategories.Endcards == "" || feed.SponsorBlockCategories.Endcards == "default" {
			feed.SponsorBlockCategories.Endcards = c.SponsorBlock.SponsorBlockCategories.Endcards
		}

		if feed.SponsorBlockCategories.InteractionReminders == "" || feed.SponsorBlockCategories.InteractionReminders == "default" {
			feed.SponsorBlockCategories.InteractionReminders = c.SponsorBlock.SponsorBlockCategories.InteractionReminders
		}

		if feed.SponsorBlockCategories.SelfPromotions == "" || feed.SponsorBlockCategories.SelfPromotions == "default" {
			feed.SponsorBlockCategories.SelfPromotions = c.SponsorBlock.SponsorBlockCategories.SelfPromotions
		}

		if feed.SponsorBlockCategories.NonmusicSections == "" || feed.SponsorBlockCategories.NonmusicSections == "default" {
			feed.SponsorBlockCategories.NonmusicSections = c.SponsorBlock.SponsorBlockCategories.NonmusicSections
		}
	}
}
