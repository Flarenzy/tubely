package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
	"io"
	"log"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strconv"
)

func processVideoForFastStart(filePath string) (string, error) {
	// cmd he command is ffmpeg and the arguments are -i,
	// the input file path, -c, copy, -movflags, faststart, -f, mp4 and the output file path.
	args := []string{"-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", filePath + ".processing"}
	cmd := exec.Command("ffmpeg", args...)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return filePath + ".processing", nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	// cmd ffprobe -v error -print_format json -show_streams samples/boots-video-horizontal.mp4
	args := []string{"-v", "error", "-print_format", "json", "-show_streams", filePath}
	getVideoMetaDataCmd := exec.Command("ffprobe", args...)
	var bytesBuffer bytes.Buffer
	getVideoMetaDataCmd.Stdout = bufio.NewWriter(&bytesBuffer)
	err := getVideoMetaDataCmd.Run()
	if err != nil {
		return "", err
	}
	var videoMetaData VideoMetaData
	err = json.Unmarshal(bytesBuffer.Bytes(), &videoMetaData)
	if err != nil {
		return "", err
	}
	aspectRatio := float64(videoMetaData.Streams[0].Width) / float64(videoMetaData.Streams[0].Height)

	if aspectRatio > (16.0/9.0)-0.1 && aspectRatio < (16.0/9.0)+0.1 {
		return "16:9", nil
	} else if aspectRatio > (9.0/16.0)-0.1 && aspectRatio < (9.0/16.0)+0.1 {
		return "9:16", nil
	}
	return "other", nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 10 << 30
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't find video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload this video", err)
		return
	}
	videoFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", err)
		return
	}
	defer func(videoFile multipart.File) {
		err := videoFile.Close()
		if err != nil {
			log.Printf("Error closing file: %v", err)
		}
	}(videoFile)
	mType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(mType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid MediaType", err)
		return
	}
	mimetype, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid MediaType", err)
		return
	}
	if mimetype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid MediaType", err)
		return
	}

	randInt, err := rand.Int(rand.Reader, big.NewInt(uploadLimit))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating random video", err)
		return
	}
	intRandInt := int(randInt.Int64())
	tempVideoFile, err := os.CreateTemp("/tmp", "temp_video"+strconv.Itoa(intRandInt))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp video", err)
		return
	}
	defer os.Remove(tempVideoFile.Name())
	defer tempVideoFile.Close()
	_, err = io.Copy(tempVideoFile, videoFile)
	_, err = tempVideoFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error seeking temp video", err)
		return
	}
	randomBuf := make([]byte, 32)
	_, err = rand.Read(randomBuf)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random string", err)
		return
	}
	ratio, err := getVideoAspectRatio(tempVideoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	var aspectRatioPrefix string
	switch ratio {
	case "16:9":
		aspectRatioPrefix = "landscape"
	case "9:16":
		aspectRatioPrefix = "portrait"
	case "other":
		aspectRatioPrefix = "other"
	default:
		respondWithError(w, http.StatusBadRequest, "Invalid Aspect Ratio", err)
		return
	}
	randomHexString := hex.EncodeToString(randomBuf)
	var s3PutObjectInput *s3.PutObjectInput
	bucket := cfg.s3Bucket
	key := aspectRatioPrefix + "/" + fmt.Sprintf("%v.ext", randomHexString)
	processedTempFilePath, err := processVideoForFastStart(tempVideoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	processedTempFile, err := os.Open(processedTempFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed temp file", err)
		return
	}
	defer os.Remove(processedTempFile.Name())
	defer processedTempFile.Close()
	s3PutObjectInput = &s3.PutObjectInput{
		Bucket:      &bucket,
		Key:         &key,
		Body:        processedTempFile,
		ContentType: &mimetype,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), s3PutObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video", err)
		return
	}
	newVideoUrl := fmt.Sprintf("%v/%v", cfg.s3CfDistribution, key)
	err = cfg.db.UpdateVideo(database.Video{
		ID:           videoID,
		VideoURL:     &newVideoUrl,
		ThumbnailURL: video.ThumbnailURL,
		CreateVideoParams: database.CreateVideoParams{
			Title:       video.Title,
			Description: video.Description,
			UserID:      userID,
		},
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video", err)
		return
	}
	video, err = cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video", err)
		return
	}

	log.Printf("Successfully uploaded video: %v, to s3 with key: %v", videoID, key)
	w.Header().Set("Content-Type", "application/octet-stream")
	respondWithJSON(w, http.StatusCreated, video)
}

type VideoMetaData struct {
	Streams []struct {
		Index              int    `json:"index"`
		CodecName          string `json:"codec_name,omitempty"`
		CodecLongName      string `json:"codec_long_name,omitempty"`
		Profile            string `json:"profile,omitempty"`
		CodecType          string `json:"codec_type"`
		CodecTagString     string `json:"codec_tag_string"`
		CodecTag           string `json:"codec_tag"`
		Width              int    `json:"width,omitempty"`
		Height             int    `json:"height,omitempty"`
		CodedWidth         int    `json:"coded_width,omitempty"`
		CodedHeight        int    `json:"coded_height,omitempty"`
		ClosedCaptions     int    `json:"closed_captions,omitempty"`
		FilmGrain          int    `json:"film_grain,omitempty"`
		HasBFrames         int    `json:"has_b_frames,omitempty"`
		SampleAspectRatio  string `json:"sample_aspect_ratio,omitempty"`
		DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
		PixFmt             string `json:"pix_fmt,omitempty"`
		Level              int    `json:"level,omitempty"`
		ColorRange         string `json:"color_range,omitempty"`
		ColorSpace         string `json:"color_space,omitempty"`
		ColorTransfer      string `json:"color_transfer,omitempty"`
		ColorPrimaries     string `json:"color_primaries,omitempty"`
		ChromaLocation     string `json:"chroma_location,omitempty"`
		FieldOrder         string `json:"field_order,omitempty"`
		Refs               int    `json:"refs,omitempty"`
		IsAvc              string `json:"is_avc,omitempty"`
		NalLengthSize      string `json:"nal_length_size,omitempty"`
		Id                 string `json:"id"`
		RFrameRate         string `json:"r_frame_rate"`
		AvgFrameRate       string `json:"avg_frame_rate"`
		TimeBase           string `json:"time_base"`
		StartPts           int    `json:"start_pts"`
		StartTime          string `json:"start_time"`
		DurationTs         int    `json:"duration_ts"`
		Duration           string `json:"duration"`
		BitRate            string `json:"bit_rate,omitempty"`
		BitsPerRawSample   string `json:"bits_per_raw_sample,omitempty"`
		NbFrames           string `json:"nb_frames"`
		ExtradataSize      int    `json:"extradata_size"`
		Disposition        struct {
			Default         int `json:"default"`
			Dub             int `json:"dub"`
			Original        int `json:"original"`
			Comment         int `json:"comment"`
			Lyrics          int `json:"lyrics"`
			Karaoke         int `json:"karaoke"`
			Forced          int `json:"forced"`
			HearingImpaired int `json:"hearing_impaired"`
			VisualImpaired  int `json:"visual_impaired"`
			CleanEffects    int `json:"clean_effects"`
			AttachedPic     int `json:"attached_pic"`
			TimedThumbnails int `json:"timed_thumbnails"`
			NonDiegetic     int `json:"non_diegetic"`
			Captions        int `json:"captions"`
			Descriptions    int `json:"descriptions"`
			Metadata        int `json:"metadata"`
			Dependent       int `json:"dependent"`
			StillImage      int `json:"still_image"`
			Multilayer      int `json:"multilayer"`
		} `json:"disposition"`
		Tags struct {
			Language    string `json:"language"`
			HandlerName string `json:"handler_name"`
			VendorId    string `json:"vendor_id,omitempty"`
			Encoder     string `json:"encoder,omitempty"`
			Timecode    string `json:"timecode,omitempty"`
		} `json:"tags"`
		SampleFmt      string `json:"sample_fmt,omitempty"`
		SampleRate     string `json:"sample_rate,omitempty"`
		Channels       int    `json:"channels,omitempty"`
		ChannelLayout  string `json:"channel_layout,omitempty"`
		BitsPerSample  int    `json:"bits_per_sample,omitempty"`
		InitialPadding int    `json:"initial_padding,omitempty"`
	} `json:"streams"`
}
