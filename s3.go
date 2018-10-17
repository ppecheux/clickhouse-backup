package main

import (
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"gopkg.in/cheggaaa/pb.v1"
)

const (
	PART_SIZE = 5 * 1024 * 1024
)

type S3 struct {
	session *session.Session
	Config  *S3Config
	DryRun  bool
}

func (s *S3) Connect() (err error) {
	s.session, err = session.NewSession(
		&aws.Config{
			Credentials:      credentials.NewStaticCredentials(s.Config.AccessKey, s.Config.SecretKey, ""),
			Region:           aws.String(s.Config.Region),
			Endpoint:         aws.String(s.Config.Endpoint),
			DisableSSL:       aws.Bool(s.Config.DisableSSL),
			S3ForcePathStyle: aws.Bool(s.Config.ForcePathStyle),
		},
	)
	return
}

func (s *S3) Upload(localPath string, dstPath string) error {
	iter, filesForDelete, err := s.newSyncFolderIterator(localPath, dstPath)
	if err != nil {
		return err
	}
	var bar *pb.ProgressBar
	if !s.Config.DisableProgressBar {
		bar = pb.StartNew(len(iter.fileInfos) + iter.skipFilesCount)
		bar.Set(iter.skipFilesCount)
		defer bar.FinishPrint("Done.")
	}

	uploader := s3manager.NewUploader(s.session)
	uploader.PartSize = PART_SIZE
	var errs []s3manager.Error
	for iter.Next() {
		object := iter.UploadObject()
		if !s.DryRun {
			if _, err := uploader.UploadWithContext(aws.BackgroundContext(), object.Object); err != nil {
				s3Err := s3manager.Error{
					OrigErr: err,
					Bucket:  object.Object.Bucket,
					Key:     object.Object.Key,
				}
				errs = append(errs, s3Err)
			}
		}
		if !s.Config.DisableProgressBar {
			bar.Increment()
		}
		if object.After == nil {
			continue
		}
		if err := object.After(); err != nil {
			s3Err := s3manager.Error{
				OrigErr: err,
				Bucket:  object.Object.Bucket,
				Key:     object.Object.Key,
			}
			errs = append(errs, s3Err)
		}
	}
	if len(errs) > 0 {
		return s3manager.NewBatchError("BatchedUploadIncomplete", "some objects have failed to upload.", errs)
	}
	if err := iter.Err(); err != nil {
		return err
	}

	// Delete
	batcher := s3manager.NewBatchDelete(s.session)
	objects := []s3manager.BatchDeleteObject{}
	for _, file := range filesForDelete {
		objects = append(objects, s3manager.BatchDeleteObject{
			Object: &s3.DeleteObjectInput{
				Bucket: aws.String(s.Config.Bucket),
				Key: aws.String(file.key),
			},
		})
	}

	if err := batcher.Delete(aws.BackgroundContext(), &s3manager.DeleteObjectsIterator{Objects: objects}); err != nil {
		log.Printf("can't delete objects with: %v", err)
	}
	return nil
}

// func (s S3) Delete(s3Path string, files map[string]fileInfo) error {
// 	svc := s3.New(s.session)
// 	for _, file := range files {
// 		s3File := file.key
// 		if s.DryRun {
// 			log.Printf("Delete '%s'", s3File)
// 			continue
// 		}
// 		deleteObjects := s3.DeleteObjectsInput{
// 			Bucket: aws.String(s.Config.Bucket),
// 			Delete: &s3.Delete{
// 				Objects: []*s3.ObjectIdentifier{
// 					{
// 						Key: aws.String(s3File),
// 					},
// 				},
// 				Quiet: aws.Bool(false),
// 			},
// 		}
// 		if _, err := svc.DeleteObjectsWithContext(aws.BackgroundContext(), &deleteObjects);err != nil {
// 			log.Printf("can't delete %s with %v", s3File, err)
// 			// return fmt.Errorf("can't delete %s with %v", s3File, err)
// 		}
// 	}
// 	return nil
// }

func (s *S3) Download(s3Path string, localPath string) error {
	localFiles, err := s.getLocalFiles(localPath, s3Path)
	if err != nil {
		return fmt.Errorf("can't open '%s' with %v", localPath, err)
	}
	s3Files, err := s.getS3Files(localPath, s3Path)
	if err != nil {
		return err
	}
	var bar *pb.ProgressBar
	if !s.Config.DisableProgressBar {
		bar = pb.StartNew(len(s3Files))
		defer bar.FinishPrint("Done.")
	}
	downloader := s3manager.NewDownloader(s.session)
	for _, s3File := range s3Files {
		bar.Increment()
		if existsFile, ok := localFiles[s3File.key]; ok {
			if existsFile.size == s3File.size {
				if s3File.etag == GetEtag(existsFile.fullpath) {
					log.Printf("Skip download file '%s' already exists", s3File.key)
					// Skip download file
					continue
				}
			}
		}

		params := &s3.GetObjectInput{
			Bucket: aws.String(s.Config.Bucket),
			Key:    aws.String(path.Join(s.Config.Path, s3Path, s3File.key)),
		}
		log.Printf("Download '%s'", s3File.key)
		newFilePath := filepath.Join(localPath, s3File.key)
		newPath := filepath.Dir(newFilePath)
		if s.DryRun {
			continue
		}
		if err := os.MkdirAll(newPath, 0755); err != nil {
			return fmt.Errorf("can't create '%s' with: %v", newPath, err)
		}
		f, err := os.Create(newFilePath)
		if err != nil {
			return fmt.Errorf("can't open '%s' with %v", newFilePath, err)
		}
		if _, err := downloader.DownloadWithContext(aws.BackgroundContext(), f, params); err != nil {
			return fmt.Errorf("can't download '%s' with %v", s3File.key, err)
		}
	}
	return nil
}

// SyncFolderIterator is used to upload a given folder
// to Amazon S3.
type SyncFolderIterator struct {
	bucket         string
	fileInfos      []fileInfo
	err            error
	acl            string
	s3path         string
	skipFilesCount int
}

type fileInfo struct {
	key      string
	fullpath string
	size     int64
	etag     string
}

func (s *S3) getLocalFiles(localPath, s3Path string) (localFiles map[string]fileInfo, err error) {
	localFiles = make(map[string]fileInfo)
	err = filepath.Walk(localPath, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			filePath := filepath.ToSlash(filePath) // fix fucking Windows slashes
			key := strings.TrimPrefix(filePath, localPath)
			localFiles[key] = fileInfo{
				key:      key,
				fullpath: filePath,
				size:     info.Size(),
			}
		}
		return nil
	})
	return
}

func (s *S3) getS3Files(localPath, s3Path string) (s3Files map[string]fileInfo, err error) {
	s3Files = make(map[string]fileInfo)
	s.remotePager(s.Config.Path, false, func(page *s3.ListObjectsV2Output) {
		for _, c := range page.Contents {
			key := strings.TrimPrefix(*c.Key, path.Join(s.Config.Path, s3Path))
			if !strings.HasSuffix(key, "/") {
				s3Files[key] = fileInfo{
					key:  key,
					size: *c.Size,
					etag: *c.ETag,
				}
			}
		}
	})
	return
}

func (s *S3) newSyncFolderIterator(localPath, dstPath string) (*SyncFolderIterator, map[string]fileInfo, error) {
	existsFiles := make(map[string]fileInfo)
	s.remotePager(s.Config.Path, false, func(page *s3.ListObjectsV2Output) {
		for _, c := range page.Contents {
			if strings.HasPrefix(*c.Key, path.Join(s.Config.Path, dstPath)) {
				key := strings.TrimPrefix(*c.Key, path.Join(s.Config.Path, dstPath))
				existsFiles[key] = fileInfo{
					key:  *c.Key,
					size: *c.Size,
					etag: *c.ETag,
				}
			}
		}
	})

	localFiles := []fileInfo{}
	skipFilesCount := 0
	err := filepath.Walk(localPath, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			filePath := filepath.ToSlash(filePath) // fix fucking Windows slashes
			key := strings.TrimPrefix(filePath, localPath)
			if existFile, ok := existsFiles[key]; ok {
				delete(existsFiles, key)
				if existFile.size == info.Size() {
					if existFile.etag == GetEtag(filePath) {
						// log.Printf("File '%s' already uploaded and has the same size and etag. Skip", key)
						skipFilesCount++
						return nil
					}
				}
			}
			localFiles = append(localFiles, fileInfo{
				key:      key,
				fullpath: filePath,
				size:     info.Size(),
			})
		}
		return nil
	})

	return &SyncFolderIterator{
		bucket:         s.Config.Bucket,
		fileInfos:      localFiles,
		acl:            s.Config.ACL,
		s3path:         path.Join(s.Config.Path, dstPath),
		skipFilesCount: skipFilesCount,
	}, existsFiles, err
}

func (s *S3) remotePager(s3Path string, delim bool, pager func(page *s3.ListObjectsV2Output)) error {
	params := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.Config.Bucket), // Required
		MaxKeys: aws.Int64(1000),
	}
	if s3Path != "" && s3Path != "/" {
		params.Prefix = aws.String(s3Path)
	}
	if delim {
		params.Delimiter = aws.String("/")
	}

	wrapper := func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		pager(page)
		return true
	}

	return s3.New(s.session).ListObjectsV2Pages(params, wrapper)
}

// Next will determine whether or not there is any remaining files to
// be uploaded.
func (iter *SyncFolderIterator) Next() bool {
	return len(iter.fileInfos) > 0
}

// Err returns any error when os.Open is called.
func (iter *SyncFolderIterator) Err() error {
	return iter.err
}

// UploadObject will prep the new upload object by open that file and constructing a new
// s3manager.UploadInput.
func (iter *SyncFolderIterator) UploadObject() s3manager.BatchUploadObject {
	fi := iter.fileInfos[0]
	iter.fileInfos = iter.fileInfos[1:]
	body, err := os.Open(fi.fullpath)
	if err != nil {
		iter.err = err
	}

	extension := filepath.Ext(fi.key)
	mimeType := mime.TypeByExtension(extension)
	if mimeType == "" {
		mimeType = "binary/octet-stream"
	}
	key := path.Join(iter.s3path, fi.key)
	input := s3manager.UploadInput{
		ACL:         &iter.acl,
		Bucket:      &iter.bucket,
		Key:         &key,
		Body:        body,
		ContentType: &mimeType,
	}

	return s3manager.BatchUploadObject{
		&input,
		nil,
	}
}
func GetEtag(path string) string {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	size := len(content)
	if size <= PART_SIZE {
		hash := md5.Sum(content)
		return fmt.Sprintf("\"%x\"", hash)
	}
	parts := 0
	pos := 0
	contentToHash := make([]byte, 0)
	for size > pos {
		endpos := pos + PART_SIZE
		if endpos >= size {
			endpos = size
		}
		hash := md5.Sum(content[pos:endpos])
		contentToHash = append(contentToHash, hash[:]...)
		pos += PART_SIZE
		parts += 1
	}
	hash := md5.Sum(contentToHash)
	return fmt.Sprintf("\"%x-%d\"", hash, parts)
}