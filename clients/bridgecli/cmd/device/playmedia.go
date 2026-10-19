package device

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/rmrobinson/house/api/command"
	"github.com/spf13/cobra"
)

var (
	playMediaURL         string
	playMediaContentType string
	playMediaTitle       string
	playMediaArtist      string
	playMediaImageURL    string
	playMediaRepeat      bool
)

func init() {
	playMediaCmd.Flags().StringVar(&playMediaURL, "url", "", "absolute http(s) URL of the media to play")
	playMediaCmd.Flags().StringVar(&playMediaContentType, "contentType", "audio/mpeg", "MIME type of the media (audio/mpeg for mp3; set it to match the file)")
	playMediaCmd.Flags().StringVar(&playMediaTitle, "title", "", "optional title to display")
	playMediaCmd.Flags().StringVar(&playMediaArtist, "artist", "", "optional artist to display")
	playMediaCmd.Flags().StringVar(&playMediaImageURL, "imageURL", "", "optional http(s) URL of art to display")
	playMediaCmd.Flags().BoolVar(&playMediaRepeat, "repeat", false, "loop the media until stopped")
	playMediaCmd.MarkFlagRequired("url")
	deviceCmd.AddCommand(playMediaCmd)
}

var playMediaCmd = &cobra.Command{
	Use:   "playmedia",
	Short: "Start playing the media at a URL on a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &command.Command{
			DeviceId: id,
			Details: &command.Command_PlayMedia{PlayMedia: &command.PlayMedia{
				ContentUrl:  playMediaURL,
				ContentType: playMediaContentType,
				Title:       playMediaTitle,
				Artist:      playMediaArtist,
				ImageUrl:    playMediaImageURL,
				Repeat:      playMediaRepeat,
			}},
		}

		resp, err := client.ExecuteCommand(cmd.Context(), req)
		if err != nil {
			return err
		}

		spew.Dump(resp)

		return nil
	},
}
