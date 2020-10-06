package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	//"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type Downloader interface {
	Download(ctx context.Context, feedConfig *config.Feed, episode *model.Episode) (*ytdl.TempFile, error)
}

type Updater struct {
	config     *config.Config
	downloader Downloader
	db         db.Storage
	fs         fs.Storage
	keys       map[model.Provider]feed.KeyProvider
}

func NewUpdater(config *config.Config, downloader Downloader, db db.Storage, fs fs.Storage) (*Updater, error) {
	keys := map[model.Provider]feed.KeyProvider{}

	for name, list := range config.Tokens {
		provider, err := feed.NewKeyProvider(list)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create key provider for %q", name)
		}
		keys[name] = provider
	}

	return &Updater{
		config:     config,
		downloader: downloader,
		db:         db,
		fs:         fs,
		keys:       keys,
	}, nil
}

func (u *Updater) Update(ctx context.Context, feedConfig *config.Feed) error {
	log.WithFields(log.Fields{
		"feed_id": feedConfig.ID,
		"format":  feedConfig.Format,
		"quality": feedConfig.Quality,
	}).Infof("-> updating %s", feedConfig.URL)

	started := time.Now()

	if err := u.updateFeed(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "update failed")
	}

	if err := u.downloadEpisodes(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "download failed")
	}

	if err := u.buildXML(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "xml build failed")
	}

	if err := u.buildOPML(ctx); err != nil {
		return errors.Wrap(err, "opml build failed")
	}

	if err := u.cleanup(ctx, feedConfig); err != nil {
		log.WithError(err).Error("cleanup failed")
	}

	elapsed := time.Since(started)
	log.Infof("successfully updated feed in %s", elapsed)
	return nil
}

// updateFeed pulls API for new episodes and saves them to database
func (u *Updater) updateFeed(ctx context.Context, feedConfig *config.Feed) error {
	info, err := builder.ParseURL(feedConfig.URL)
	if err != nil {
		return errors.Wrapf(err, "failed to parse URL: %s", feedConfig.URL)
	}

	keyProvider, ok := u.keys[info.Provider]
	if !ok {
		return errors.Errorf("key provider %q not loaded", info.Provider)
	}

	// Create an updater for this feed type
	provider, err := builder.New(ctx, info.Provider, keyProvider.Get())
	if err != nil {
		return err
	}

	// Query API to get episodes
	log.Debug("building feed")
	result, err := provider.Build(ctx, feedConfig)
	if err != nil {
		return err
	}

	log.Debugf("received %d episode(s) for %q", len(result.Episodes), result.Title)

	episodeSet := make(map[string]struct{})
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status != model.EpisodeDownloaded && episode.Status != model.EpisodeCleaned {
			episodeSet[episode.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := u.db.AddFeed(ctx, feedConfig.ID, result); err != nil {
		return err
	}

	for _, episode := range result.Episodes {
		delete(episodeSet, episode.ID)
	}

	// removing episodes that are no longer available in the feed and not downloaded or cleaned
	for id := range episodeSet {
		log.Infof("removing episode %q", id)
		err := u.db.DeleteEpisode(feedConfig.ID, id)
		if err != nil {
			return err
		}
	}

	log.Debug("successfully saved updates to storage")
	return nil
}

func (u *Updater) matchRegexpFilter(pattern, str string, negative bool, logger log.FieldLogger) bool {
	if pattern != "" {
		matched, err := regexp.MatchString(pattern, str)
		if err != nil {
			logger.Warnf("pattern %q is not a valid")
		} else {
			if matched == negative {
				logger.Infof("skipping due to mismatch")
				return false
			}
		}
	}
	return true
}

func (u *Updater) matchFilters(episode *model.Episode, filters *config.Filters) bool {
	logger := log.WithFields(log.Fields{"episode_id": episode.ID})
	if !u.matchRegexpFilter(filters.Title, episode.Title, false, logger.WithField("filter", "title")) {
		return false
	}
	if !u.matchRegexpFilter(filters.NotTitle, episode.Title, true, logger.WithField("filter", "not_title")) {
		return false
	}

	if !u.matchRegexpFilter(filters.Description, episode.Description, false, logger.WithField("filter", "description")) {
		return false
	}
	if !u.matchRegexpFilter(filters.NotDescription, episode.Description, true, logger.WithField("filter", "not_description")) {
		return false
	}

	return true
}

func (u *Updater) downloadEpisodes(ctx context.Context, feedConfig *config.Feed) error {
	var (
		feedID       = feedConfig.ID
		downloadList []*model.Episode
		pageSize     = feedConfig.PageSize
	)

	log.WithField("page_size", pageSize).Info("downloading episodes")

	// Build the list of files to download
	if err := u.db.WalkEpisodes(ctx, feedID, func(episode *model.Episode) error {
		if episode.Status != model.EpisodeNew && episode.Status != model.EpisodeError {
			// File already downloaded
			return nil
		}

		if !u.matchFilters(episode, &feedConfig.Filters) {
			return nil
		}

		// Limit the number of episodes downloaded at once
		pageSize--
		if pageSize <= 0 {
			return nil
		}

		log.Debugf("adding %s (%q) to queue", episode.ID, episode.Title)
		downloadList = append(downloadList, episode)
		return nil
	}); err != nil {
		return errors.Wrapf(err, "failed to build update list")
	}

	var (
		downloadCount = len(downloadList)
		downloaded    = 0
	)

	if downloadCount > 0 {
		log.Infof("download count: %d", downloadCount)
	} else {
		log.Info("no episodes to download")
		return nil
	}

	// Download pending episodes

	for idx, episode := range downloadList {
		var (
			logger      = log.WithFields(log.Fields{"index": idx, "episode_id": episode.ID})
			episodeName = feed.EpisodeName(feedConfig, episode)
		)

		// Check whether episode already exists
		size, err := u.fs.Size(ctx, feedID, episodeName)
		if err == nil {
			logger.Infof("episode %q already exists on disk", episode.ID)

			// File already exists, update file status and disk size
			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Size = size
				episode.Status = model.EpisodeDownloaded
				return nil
			}); err != nil {
				logger.WithError(err).Error("failed to update file info")
				return err
			}

			continue
		} else if os.IsNotExist(err) {
			// Will download, do nothing here
		} else {
			logger.WithError(err).Error("failed to stat file")
			return err
		}

		type Segment struct {
			Segment  []float64 `json:"segment"`
			UUID     string
			Category string `json:"category"`
		}

		var segments []Segment

		// Do sponsorblock stuffs
		timeSincePosted := time.Since(episode.PubDate)
		delayPassed := timeSincePosted.Microseconds() > feedConfig.SponsorblockDelay.Microseconds()

		logger.Debugf("SponsorblockMode is %s", feedConfig.SponsorblockMode)
		if feedConfig.SponsorblockMode == "delay" && !delayPassed {
			logger.Info("Sponsorblock mode is delay and configured delay has not passed yet: Skipping download of this episode and segments query for now")
		}

		if feedConfig.SponsorblockMode != "off" {
			url := u.config.SponsorBlock.ApiUrl + fmt.Sprintf("/api/skipSegments?categories=[\"sponsor\",\"intro\",\"outro\",\"interaction\",\"selfpromo\",\"music_offtopic\"]&videoID=%s", episode.ID)
			logger.Debugf("Grabbing url %s", url)
			resp, err := http.Get(url)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == 404 {
					logger.Info("No sponsor segments available yet")
				} else if resp.StatusCode == 200 {
					data, err := ioutil.ReadAll(resp.Body)
					if err != nil {
						logger.WithError(err).Error("Failed reading body of sponsorblock response")
					} else {
						logger.Debugf("Sponsorblock responded with json %#v", string(data))
						if err := json.Unmarshal(data, &segments); err != nil {
							logger.WithError(err).Error("Failed parsing json")
						}
					}
				} else {
					logger.WithError(err).Errorf("Sponsorblock server returned unexpected error %d", resp.StatusCode)
				}
			} else {
				logger.WithError(err).Warn("failed to retrieve sponsor segments from sponsorblock server")
			}
		}

		if feedConfig.SponsorblockMode == "require" && len(segments) == 0 {
			logger.Info("Sponsorblock mode is require and zero segments have been found: Skipping download of this episode for now")
			continue
		}
		if feedConfig.SponsorblockMode == "requiredelay" && len(segments) == 0 && !delayPassed {
			logger.Info("Sponsorblock mode is requiredelay, zero segments have been found, and configured delay has not passed yet: Skipping download of this episode for now")
			continue
		}

		// Download episode to disk
		// We download the episode to a temp directory first to avoid clients downloading this file
		// while still being processed by youtube-dl (e.g. a file is being downloaded from YT or encoding in progress)

		logger.Infof("! downloading episode %s", episode.VideoURL)
		tempFile, err := u.downloader.Download(ctx, feedConfig, episode)
		if err != nil {
			// YouTube might block host with HTTP Error 429: Too Many Requests
			// We still need to generate XML, so just stop sending download requests and
			// retry next time
			if err == ytdl.ErrTooManyRequests {
				logger.Warn("server responded with a 'Too Many Requests' error")
				break
			}

			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Status = model.EpisodeError
				return nil
			}); err != nil {
				return err
			}

			continue
		}

		var fileSize int64
		logger.Debugf("Segments from sponsorblock: %#v", segments)
		if len(segments) == 0 {
			logger.Debug("copying file")
			var err error
			fileSize, err = u.fs.Create(ctx, feedID, episodeName, tempFile)
			tempFile.Close()
			if err != nil {
				logger.WithError(err).Error("failed to copy file")
				return err
			}
		} else {
			logger.Debug("in file is %#v", tempFile)
			// time.Sleep(time.Duration(10) * time.Minute)
			// Time to get trimmin'

			// First, use the list of segments (time ranges to drop) to make a list of "keeps" (time ranges to keep)
			var keeps [][2]float64
			c := feedConfig.SponsorBlockCategories
			nextStart := 0.0
			for _, segment := range segments {
				if segment.Category == "sponsor" && c.Sponsors == "keep" {
					continue
				}
				if segment.Category == "intro" && c.Intermissions == "keep" {
					continue
				}
				if segment.Category == "outro" && c.Endcards == "keep" {
					continue
				}
				if segment.Category == "interaction" && c.InteractionReminders == "keep" {
					continue
				}
				if segment.Category == "selfpromo" && c.SelfPromotions == "keep" {
					continue
				}
				if segment.Category == "music_offtopic" && c.NonmusicSections == "keep" {
					continue
				}
				keeps = append(keeps, [2]float64{nextStart, segment.Segment[0]})
				nextStart = segment.Segment[1]
			}
			keeps = append(keeps, [2]float64{nextStart, -1})
			logger.Debugf("'Keep' segments are %#v", keeps)

			tmpDir, err := ioutil.TempDir("", "podsync-ffmpeg-")
			if err != nil {
				return errors.Wrap(err, "failed to get temp dir for ffmpeg")
			}
			// defer func() {
			// 	if err != nil {
			// 		err1 := os.RemoveAll(tmpDir)
			// 		if err1 != nil {
			// 			log.Errorf("could not remove temp dir: %v", err1)
			// 		}
			// 	}
			// }()

			ext := "mp4"
			videoStreams := 1
			if feedConfig.Format == model.FormatAudio {
				ext = "mp3"
				videoStreams = 0
			}

			//var segmentFiles []string
			var filter string
			var finalFilter string
			for idx, segment := range keeps {
				// [0:v]trim=start=0:end=30,setpts=PTS-STARTPTS[s1v];[0:a]atrim=start=0:end=30,asetpts=PTS-STARTPTS[s1a];
				start, end := segment[0], segment[1]
				filter += fmt.Sprintf("[0:a]atrim=start=%f", start)
				if end >= 0 {
					filter += fmt.Sprintf(":end=%f", end)
				}
				filter += fmt.Sprintf(",asetpts=PTS-STARTPTS[s%da];", idx)
				if feedConfig.Format != model.FormatAudio {
					filter += fmt.Sprintf("[0:v]trim=start=%f", start)
					if end >= 0 {
						filter += fmt.Sprintf(":end=%f", end)
					}
					filter += fmt.Sprintf(",setpts=PTS-STARTPTS[s%dv];", idx)
					finalFilter += fmt.Sprintf("[s%dv]", idx)
				}
				finalFilter += fmt.Sprintf("[s%da]", idx)
				/*filePath := filepath.Join(tmpDir, fmt.Sprintf("%d.%s", idx, ext))
				ctx := exec.Command("ffmpeg", "-ss", fmt.Sprintf("%f", start), "-t", fmt.Sprintf("%f", end-start), "-i", tempFile.FullPath(), filePath)
				err := ctx.Run()
				if err != nil {
					return errors.Wrap(err, "Failed trying to run ffmpeg command")
				}
				segmentFiles = append(segmentFiles, filePath)*/
			}
			filter += finalFilter + fmt.Sprintf("concat=n=%d:v=%d:a=1", len(keeps), videoStreams)
			if feedConfig.Format != model.FormatAudio {
				filter += "[outv]"
			}
			filter += "[outa]"
			processedPath := filepath.Join(tmpDir, fmt.Sprintf("processed-%s.%s", episode.ID, ext))
			args := []string{"-f", ext, "-i", tempFile.Fullpath(), "-filter_complex", filter, "-map", "[outa]"}
			if feedConfig.Format != model.FormatAudio {
				args = append(args, "-map", "[outv]")
			}
			args = append(args, processedPath)
			logger.Debugf("Calling ffmpeg with args %#v", args)
			cmd := exec.Command("ffmpeg", args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			//cmd.Stdin = tempFile.File
			// pipe, err := cmd.StdinPipe()
			// if err != nil {
			// 	return errors.Wrap(err, "Error running ffmpeg")
			// }
			//err = cmd.Run()
			err = cmd.Start()
			if err != nil {
				return errors.Wrap(err, "Error running ffmpeg")
			}
			//_, err2 := io.Copy(pipe, tempFile.File)
			err = cmd.Wait()
			tempFile.Close()
			//logger.Debug("ffmpeg stdout", cmd.S)
			if err != nil {
				return errors.Wrap(err, "Error running ffmpeg")
			}
			// if err2 != nil {
			// 	return errors.Wrap(err2, "Error running ffmpeg")
			// }

			logger.Debug("copying cut file %s", processedPath)
			tempFileProcessed, err := os.Open(processedPath)
			if err == nil {
				fileSize, err = u.fs.Create(ctx, feedID, episodeName, tempFileProcessed)
			}
			tempFile.Close()
			if err != nil {
				logger.WithError(err).Error("failed to copy file")
				return err
			}
		}

		// Update file status in database

		logger.Infof("successfully downloaded file %q", episode.ID)
		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Size = fileSize
			episode.Status = model.EpisodeDownloaded
			return nil
		}); err != nil {
			return err
		}

		downloaded++
	}

	log.Infof("downloaded %d episode(s)", downloaded)
	return nil
}

func (u *Updater) buildXML(ctx context.Context, feedConfig *config.Feed) error {
	f, err := u.db.GetFeed(ctx, feedConfig.ID)
	if err != nil {
		return err
	}

	// Build iTunes XML feed with data received from builder
	log.Debug("building iTunes podcast feed")
	podcast, err := feed.Build(ctx, f, feedConfig, u.fs)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(podcast.String()))
		xmlName = fmt.Sprintf("%s.xml", feedConfig.ID)
	)

	if _, err := u.fs.Create(ctx, "", xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload new XML feed")
	}

	return nil
}

func (u *Updater) buildOPML(ctx context.Context) error {
	// Build OPML with data received from builder
	log.Debug("building podcast OPML")
	opml, err := feed.BuildOPML(ctx, u.config, u.db, u.fs)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(opml))
		xmlName = fmt.Sprintf("%s.opml", "podsync")
	)

	if _, err := u.fs.Create(ctx, "", xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload OPML")
	}

	return nil
}

func (u *Updater) cleanup(ctx context.Context, feedConfig *config.Feed) error {
	var (
		feedID = feedConfig.ID
		logger = log.WithField("feed_id", feedID)
		count  = feedConfig.Clean.KeepLast
		list   []*model.Episode
		result *multierror.Error
	)

	if count < 1 {
		logger.Info("nothing to clean")
		return nil
	}

	logger.WithField("count", count).Info("running cleaner")
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status == model.EpisodeDownloaded {
			list = append(list, episode)
		}
		return nil
	}); err != nil {
		return err
	}

	if count > len(list) {
		return nil
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].PubDate.After(list[j].PubDate)
	})

	for _, episode := range list[count:] {
		logger.WithField("episode_id", episode.ID).Infof("deleting %q", episode.Title)

		if err := u.fs.Delete(ctx, feedConfig.ID, feed.EpisodeName(feedConfig, episode)); err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "failed to delete episode: %s", episode.ID))
			continue
		}

		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Status = model.EpisodeCleaned
			episode.Title = ""
			episode.Description = ""
			return nil
		}); err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "failed to set state for cleaned episode: %s", episode.ID))
			continue
		}
	}

	return result.ErrorOrNil()
}
