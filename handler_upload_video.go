package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"strconv"
)

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
	randomHexString := hex.EncodeToString(randomBuf)
	var s3PutObjectInput *s3.PutObjectInput
	bucket := cfg.s3Bucket
	key := fmt.Sprintf("%v.ext", randomHexString)
	s3PutObjectInput = &s3.PutObjectInput{
		Bucket:      &bucket,
		Key:         &key,
		Body:        tempVideoFile,
		ContentType: &mimetype,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), s3PutObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video", err)
		return
	}
	newVideoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
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
	log.Printf("Successfully uploaded video: %v, to s3 with key: %v", videoID, key)
	w.Header().Set("Content-Type", "application/octet-stream")
	respondWithJSON(w, http.StatusCreated, map[string]string{})
}
