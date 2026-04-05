package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"

	"github.com/angoo/agentfoundry/internal/config"
)

type AgentVersion struct {
	VersionID    string `json:"version_id"`
	LastModified string `json:"last_modified"`
	Size         int64  `json:"size"`
	IsLatest     bool   `json:"is_latest"`
}

type S3Store struct {
	client *s3.Client
	bucket string
	prefix string
	reg    AgentRegistrar

	mu          sync.RWMutex
	definitions map[string]*config.Definition
}

type AgentRegistrar interface {
	RegisterAgent(def *config.Definition) error
}

func NewS3Store(client *s3.Client, bucket, prefix string, reg AgentRegistrar) *S3Store {
	return &S3Store{
		client:      client,
		bucket:      bucket,
		prefix:      prefix,
		reg:         reg,
		definitions: make(map[string]*config.Definition),
	}
}

func (s *S3Store) LoadAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := s.prefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return fmt.Errorf("list S3 objects: %w", err)
	}

	for _, obj := range resp.Contents {
		if !strings.HasSuffix(*obj.Key, ".yaml") && !strings.HasSuffix(*obj.Key, ".yml") {
			continue
		}

		if err := s.loadObject(ctx, *obj.Key); err != nil {
			slog.Error("failed to load definition from S3", "key", *obj.Key, "error", err)
			continue
		}
	}

	slog.Info("agent definitions loaded from S3", "count", len(s.definitions), "bucket", s.bucket, "prefix", s.prefix)
	return nil
}

func (s *S3Store) loadObject(ctx context.Context, key string) error {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("get S3 object %s: %w", key, err)
	}
	defer resp.Body.Close()

	var def config.Definition
	if err := yaml.NewDecoder(resp.Body).Decode(&def); err != nil {
		return fmt.Errorf("parse S3 object %s: %w", key, err)
	}

	if err := def.Validate(); err != nil {
		return fmt.Errorf("validate %s: %w", key, err)
	}

	if err := s.reg.RegisterAgent(&def); err != nil {
		return err
	}
	s.definitions[def.Name] = &def
	slog.Info("registered agent from S3", "name", def.Name)
	return nil
}

func (s *S3Store) SaveDefinition(def *config.Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := yaml.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal definition: %w", err)
	}

	key := s.prefix + def.Name + ".yaml"
	ctx := context.Background()

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
	})
	if err != nil {
		return fmt.Errorf("put S3 object %s: %w", key, err)
	}

	if err := s.reg.RegisterAgent(def); err != nil {
		return err
	}
	s.definitions[def.Name] = def
	return nil
}

func (s *S3Store) DeleteDefinition(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx := context.Background()

	for _, ext := range []string{".yaml", ".yml"} {
		key := s.prefix + name + ext
		_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			slog.Error("failed to delete S3 object", "key", key, "error", err)
		}
	}

	delete(s.definitions, name)
	return nil
}

func (s *S3Store) GetDefinition(name string) *config.Definition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.definitions[name]
}

func (s *S3Store) ListDefinitions() []*config.Definition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defs := make([]*config.Definition, 0, len(s.definitions))
	for _, def := range s.definitions {
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool {
		return defs[i].Name < defs[j].Name
	})
	return defs
}

func (s *S3Store) GetRawDefinition(name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ctx := context.Background()

	for _, ext := range []string{".yaml", ".yml"} {
		key := s.prefix + name + ext
		resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			slog.Error("failed to get S3 object", "key", key, "error", err)
			continue
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read S3 object %s: %w", key, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("definition %q not found", name)
}

func (s *S3Store) SaveRawDefinition(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var def config.Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	if err := def.Validate(); err != nil {
		return err
	}

	filename := def.Name + ".yaml"
	key := s.prefix + filename
	ctx := context.Background()

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
	})
	if err != nil {
		return fmt.Errorf("put S3 object %s: %w", key, err)
	}

	if err := s.reg.RegisterAgent(&def); err != nil {
		return err
	}
	if name != def.Name {
		for _, ext := range []string{".yaml", ".yml"} {
			oldKey := s.prefix + name + ext
			_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(oldKey),
			})
			if err != nil {
				slog.Error("failed to delete old S3 object on rename", "key", oldKey, "error", err)
			}
		}
		delete(s.definitions, name)
	}
	s.definitions[def.Name] = &def
	return nil
}

func (s *S3Store) ListVersions(ctx context.Context, name string) ([]AgentVersion, error) {
	key := s.prefix + name + ".yaml"

	resp, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("list versions for %s: %w", name, err)
	}

	var versions []AgentVersion
	for _, v := range resp.Versions {
		if v.Key == nil || *v.Key != key {
			continue
		}
		av := AgentVersion{
			VersionID:    aws.ToString(v.VersionId),
			LastModified: aws.ToTime(v.LastModified).Format("2006-01-02T15:04:05Z"),
			Size:         aws.ToInt64(v.Size),
		}
		if v.IsLatest != nil {
			av.IsLatest = *v.IsLatest
		}
		versions = append(versions, av)
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].LastModified > versions[j].LastModified
	})

	return versions, nil
}

func (s *S3Store) GetVersion(ctx context.Context, name, versionID string) ([]byte, *config.Definition, error) {
	key := s.prefix + name + ".yaml"

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get version %s of %s: %w", versionID, name, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read version %s of %s: %w", versionID, name, err)
	}

	var def config.Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return data, nil, fmt.Errorf("parse version: %w", err)
	}

	return data, &def, nil
}

func (s *S3Store) Rollback(ctx context.Context, name, versionID string) error {
	key := s.prefix + name + ".yaml"
	copySource := s.bucket + "/" + key + "?versionId=" + versionID

	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("rollback %s to version %s: %w", name, versionID, err)
	}

	return s.loadObject(ctx, key)
}
