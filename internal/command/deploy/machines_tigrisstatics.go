package deploy

import (
	"context"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/samber/lo"
	"github.com/superfly/fly-go"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/appconfig"
	extensions "github.com/superfly/flyctl/internal/command/extensions/core"
	flyconfig "github.com/superfly/flyctl/internal/config"
	"github.com/superfly/flyctl/internal/flyutil"
	"github.com/superfly/flyctl/internal/haikunator"
	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/terminal"
	"github.com/superfly/macaroon/flyio"
	"github.com/superfly/macaroon/resset"
	"github.com/superfly/tokenizer"
)

const (
	tigrisHostname = "fly.storage.tigris.dev"
	// This URL is intentionally HTTP. We're funneling through tokenizer over HTTPS because:
	//  1. This connection can not be HTTPS, because we're injecting authentication headers into the requests
	//  2. This connection is still secure, because the connection *to* tokenizer is over HTTPS, and tokenizer
	//     will forward requests upstream with HTTPS.
	tigrisUrl = "http://" + tigrisHostname

	tokenizerUrl     = "https://tokenizer.fly.io"
	tokenizerSealKey = "3afdb665d93f741adc98a6cfecb36f1e02403a095e8efa921fd2321857011f42"

	staticsMetaKeyAppId      = "fly-statics-app-id"
	staticsMetaTokenizedAuth = "fly-statics-tokenized-auth"
)

// TODO(allison): Delete the statics bucket when the app is deleted.

const staticsKeepVersions = 3

type tigrisStaticsData struct {
	s3              *s3.Client
	bucket          string
	root            string
	originalStatics []appconfig.Static
}

func (md *machineDeployment) staticsUseTigris(ctx context.Context) bool {

	for _, static := range md.appConfig.Statics {
		if staticIsCandidateForTigrisPush(static) {
			return true
		}
	}

	return false
}

func (md *machineDeployment) staticsEnsureBucketCreated(ctx context.Context) (tokenizedAuth string, retErr error) {

	client := flyutil.ClientFromContext(ctx)
	gqlClient := client.GenqClient()

	response, err := gql.ListAddOns(ctx, gqlClient, "tigris")
	if err != nil {
		return "", err
	}

	for _, extension := range response.AddOns.Nodes {
		if extension.Metadata == nil {
			continue
		}
		meta := extension.Metadata.(map[string]interface{})
		if meta[staticsMetaKeyAppId] == md.app.ID {
			md.tigrisStatics.bucket = extension.Name
			return meta[staticsMetaTokenizedAuth].(string), nil
		}
	}

	org, err := client.GetOrganizationBySlug(ctx, md.app.Organization.Slug)
	if err != nil {
		return "", err
	}

	bucketName := fmt.Sprintf("%s-statics", md.appConfig.AppName)

	params := extensions.ExtensionParams{
		Organization:         org,
		Provider:             "tigris",
		Options:              gql.AddOnOptions{},
		ErrorCaptureCallback: nil,
		OverrideRegion:       md.appConfig.PrimaryRegion,
		OverrideName:         &bucketName,
	}
	params.Options["website"] = map[string]interface{}{
		"domain_name": "",
	}
	params.Options["accelerate"] = false
	// TODO(allison): Make sure we still need this when virtual services drop :)
	params.Options["public"] = true

	ext, err := extensions.ProvisionExtension(ctx, params)
	if err != nil {
		// If the extension name is taken, try again, haikunating the name.
		// If that fails too, return the original error. Otherwise, continue successfully
		if strings.Contains(err.Error(), "already exists for app") ||
			strings.Contains(err.Error(), "unavailable for creation") {
			bucketName = fmt.Sprintf("%s-%s", *params.OverrideName, haikunator.Haikunator().String())
			params.OverrideName = &bucketName
			newExt, newErr := extensions.ProvisionExtension(ctx, params)
			if newErr == nil {
				ext = newExt
				err = nil
			}
		}
	}
	if err != nil {
		return "", err
	}

	md.tigrisStatics.bucket = bucketName

	defer func() {
		if retErr != nil {
			client := flyutil.ClientFromContext(ctx).GenqClient()
			// Using context.Background() here in case the error is that the context is canceled.
			_, err := gql.DeleteAddOn(context.Background(), client, bucketName)
			if err != nil {
				fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Failed to delete extension: %v\n", err)
			}
		}
	}()

	secrets := ext.Data.Environment.(map[string]interface{})

	tokenizedKey, err := md.staticsTokenizeTigrisSecrets(ctx, org, secrets)
	if err != nil {
		return "", err
	}

	// TODO(allison): I'd really like ProvisionExtension to return the extension's ID, but for now we can just refetch it
	extFull, err := gql.GetAddOn(ctx, client.GenqClient(), bucketName, string(gql.AddOnTypeTigris))

	// Update the addon with the tokenized key and the name of the app
	_, err = gql.UpdateAddOn(ctx, client.GenqClient(), extFull.AddOn.Id, extFull.AddOn.AddOnPlan.Id, []string{}, extFull.AddOn.Options, map[string]interface{}{
		staticsMetaKeyAppId:      md.app.ID,
		staticsMetaTokenizedAuth: tokenizedKey,
	})
	if err != nil {
		return "", err
	}
	return tokenizedKey, nil
}

func (md *machineDeployment) staticsTokenizeTigrisSecrets(
	ctx context.Context,
	org *fly.Organization,
	secrets map[string]interface{},
) (string, error) {

	client := flyutil.ClientFromContext(ctx)

	orgId, err := strconv.ParseUint(org.InternalNumericID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to decode org ID for %s: %w", org.Slug, err)
	}

	// TODO(allison): We should just pass the whole app to deploy instead of re-grabbing it here
	app, err := client.GetApp(ctx, md.app.Name)
	if err != nil {
		return "", err
	}
	appId := uint64(app.InternalNumericID)

	// TODO(allison): How do we handle moving an app between orgs?
	//                We're locking this token behind a hard dependency on the App ID and Org ID, but the Org ID
	//                will change when moving from one org to another.
	secret := &tokenizer.Secret{
		AuthConfig: &tokenizer.FlyioMacaroonAuthConfig{Access: flyio.Access{
			Action: resset.ActionWrite,
			OrgID:  &orgId,
			AppID:  &appId,
		}},
		ProcessorConfig: &tokenizer.Sigv4ProcessorConfig{
			AccessKey: secrets["AWS_ACCESS_KEY_ID"].(string),
			SecretKey: secrets["AWS_SECRET_ACCESS_KEY"].(string),
		},
		RequestValidators: []tokenizer.RequestValidator{tokenizer.AllowHosts(fmt.Sprintf("%s.%s", md.tigrisStatics.bucket, tigrisHostname))},
	}

	return secret.Seal(tokenizerSealKey)
}

// Create the tigris bucket if not created.
func (md *machineDeployment) staticsInitialize(ctx context.Context) error {

	tokenizedAuth, err := md.staticsEnsureBucketCreated(ctx)
	if err != nil {
		return err
	}

	// NOTE: This statics definition in the release sent to our API
	//       should be correct and unmodified. *But*, because we're
	//       modifying the app config in-place to ensure we don't have
	//       double definitions for the static (both tigris & from local),
	//       we'll pull an incorrect config if we grab it from machines.
	//
	// TODO(allison): We can probably solve this by sending the full statics config
	//                to each machine as metadata and resynthesizing it during config save.
	md.tigrisStatics.originalStatics = md.appConfig.Statics
	md.appConfig.Statics = lo.Filter(md.appConfig.Statics, func(static appconfig.Static, _ int) bool {
		return !staticIsCandidateForTigrisPush(static)
	})

	s3Config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("tokenizer-access-key", "tokenizer-secret-key", "")),
		awsconfig.WithRegion("auto"),
	)
	if err != nil {
		return err
	}
	s3Config.BaseEndpoint = fly.Pointer(tigrisUrl)

	parsedProxyUrl, err := url.Parse(tokenizerUrl)
	if err != nil {
		// Should be impossible, this is not runtime-controlled and issues would be caught before release.
		return fmt.Errorf("could not parse tokenizer URL: %w", err)
	}
	s3HttpTransport := http.DefaultTransport.(*http.Transport).Clone()
	s3HttpTransport.Proxy = http.ProxyURL(parsedProxyUrl)

	cfg := flyconfig.FromContext(ctx)
	// TODO(allison): This works for development, but this isn't guaranteed to provide macaroons.
	//                Ask ben how we can consistently get a macaroon for the current user.
	userAuthHeader := cfg.Tokens.GraphQLHeader()

	s3HttpClient, err := tokenizer.Client(tokenizerUrl, tokenizer.WithAuth(userAuthHeader), tokenizer.WithSecret(tokenizedAuth, map[string]string{}))
	if err != nil {
		return err
	}

	s3Config.HTTPClient = s3HttpClient

	md.tigrisStatics.s3 = s3.NewFromConfig(s3Config)

	md.tigrisStatics.root = fmt.Sprintf("fly-statics/%s/%d", md.appConfig.AppName, md.releaseVersion)
	return nil
}

func staticIsCandidateForTigrisPush(static appconfig.Static) bool {
	if static.TigrisBucket != "" {
		// If this is already mapped to a tigris bucket, that means the user is directly
		// controlling the bucket, and therefore we should not touch it or push anything to it.
		return false
	}
	if len(static.GuestPath) == 0 {
		return false
	}
	// TODO(allison): Extract statics from the docker image?
	if static.GuestPath[0] == '/' {
		// This is an absolute path. We should not modify this, as this path
		// is going to be relative to the root of the docker image.
		return false
	}
	// Now we know that we have a relative path, and that we're not already using a tigris bucket.
	// We can push this to the bucket.
	return true
}

// Upload a directory to the tigris bucket with the given prefix `dest`.
func (client *tigrisStaticsData) uploadDirectory(ctx context.Context, dest, localPath string) error {

	// Clean the destination path.
	// This is for the case where someone launches an app, it fails, then they
	// just delete the app and re-launch it.
	if err := client.deleteDirectory(ctx, dest); err != nil {
		return err
	}

	// Recursively upload the directory to the bucket.
	var files []string
	localDir := os.DirFS(localPath)
	err := fs.WalkDir(localDir, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, name)
		return nil
	})
	if err != nil {
		return err
	}

	// Create a work queue, then start a number of workers to upload the files.
	workQueue := make(chan string, len(files))
	for _, file := range files {
		workQueue <- file
	}
	close(workQueue)

	workerErr := make(chan error, 1)
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	wg := sync.WaitGroup{}
	defer cancelWorkers()

	worker := func() error {
		defer wg.Done()
		for file := range workQueue {

			reader, err := os.Open(filepath.Join(localPath, file))
			if err != nil {
				return err
			}

			mimeType := "application/octet-stream"
			if detectedMime := mime.TypeByExtension(filepath.Ext(file)); detectedMime != "" {
				mimeType = detectedMime
			} else {
				first512 := make([]byte, 512)
				_, err = reader.Read(first512)
				if err != nil {
					return fmt.Errorf("failed to read static file %s: %w", file, err)
				} else {
					_, err = reader.Seek(0, 0)
					if err != nil {
						return fmt.Errorf("failed to seek static file %s: %w", file, err)
					}
					mimeType = http.DetectContentType(first512)
				}
			}

			if runtime.GOOS == "windows" {
				file = strings.ReplaceAll(file, "\\", "/")
			}

			terminal.Debugf("Uploading to %s\n", path.Join(dest, file))

			// Upload the file to the bucket.
			_, err = client.s3.PutObject(workerCtx, &s3.PutObjectInput{
				Bucket:      &client.bucket,
				Key:         fly.Pointer(path.Join(dest, file)),
				Body:        reader,
				ContentType: &mimeType,
			})
			if err != nil {
				return err
			}

			err = reader.Close()
			if err != nil {
				terminal.Debugf("failed to close file %s: %v", file, err)
			}
		}
		return nil
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			err := worker()
			if err != nil {
				workerErr <- err
				cancelWorkers()
			}
		}()
	}

	wg.Wait()

	// Check if any of the workers failed.
	select {
	case err := <-workerErr:
		return err
	default:
		return nil
	}
}

// Delete all files with the given prefix `dir` from the bucket.
func (client *tigrisStaticsData) deleteDirectory(ctx context.Context, dir string) error {

	if runtime.GOOS == "windows" {
		dir = strings.ReplaceAll(dir, "\\", "/")
	}

	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}

	// List all files in the bucket with the given prefix.
	listOutput, err := client.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: &client.bucket,
		Prefix: fly.Pointer(dir),
	})
	if err != nil {
		return err
	}

	objectIdentifiers := lo.Map(listOutput.Contents, func(obj types.Object, _ int) types.ObjectIdentifier {
		return types.ObjectIdentifier{
			Key: obj.Key,
		}
	})

	// Delete files in batches of 1000
	split := lo.Chunk(objectIdentifiers, 1000)
	for _, batch := range split {

		_, err = client.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &client.bucket,
			Delete: &types.Delete{
				Objects: batch,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (client *tigrisStaticsData) deleteOldStatics(ctx context.Context, appName string, currentVer int) error {

	// List directories in the app's directory.
	// Delete all versions except for the three latest versions.

	// TODO(allison): Support pagination if the bucket contains >1k objects.
	//                Right now, this is egregiously incorrect and brittle.
	// List `fly-statics/<app_name>/` to get a list of all versions.
	listOutput, err := client.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    &client.bucket,
		Prefix:    fly.Pointer(fmt.Sprintf("fly-statics/%s/", appName)),
		Delimiter: fly.Pointer("/"),
	})

	if err != nil {
		return err
	}

	// Extract the version numbers from the common prefixes.
	// These should be strings of the format `fly-statics/<app_name>/<version>/`.
	versions := lo.FilterMap(listOutput.CommonPrefixes, func(prefix types.CommonPrefix, _ int) (int, bool) {
		// The number is the third part of the prefix.
		parts := strings.Split(*prefix.Prefix, "/")
		if len(parts) < 3 {
			return 0, false
		}
		num, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, false
		}
		return num, true
	})

	var ignore []int
	for _, version := range versions {
		if version > currentVer {
			ignore = append(ignore, version)
			terminal.Debugf("Deleting too-new static dir (likely for reused app name): %s\n", fmt.Sprintf("fly-statics/%s/%d/", appName, version))
			err = client.deleteDirectory(ctx, fmt.Sprintf("fly-statics/%s/%d/", appName, version))
			if err != nil {
				return err
			}
		}
	}

	versions = lo.Filter(versions, func(version int, _ int) bool {
		return !lo.Contains(ignore, version)
	})

	// Sort the numbers in ascending order.
	slices.Sort(versions)

	versions = lo.Uniq(versions)

	// Delete versions that are older than we wish to keep.
	if len(versions) > staticsKeepVersions {
		versions = versions[:len(versions)-staticsKeepVersions]
		for _, version := range versions {
			terminal.Debugf("Deleting old static dir: %s\n", fmt.Sprintf("fly-statics/%s/%d/", appName, version))
			err = client.deleteDirectory(ctx, fmt.Sprintf("fly-statics/%s/%d/", appName, version))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Push statics to the tigris bucket.
func (md *machineDeployment) staticsPush(ctx context.Context) (err error) {

	defer func() {
		panicErr := recover()
		if err != nil || panicErr != nil {
			md.staticsCleanupAfterFailure()
		}
		if panicErr != nil {
			panic(panicErr)
		}
	}()

	staticNum := 0
	for _, static := range md.tigrisStatics.originalStatics {
		if !staticIsCandidateForTigrisPush(static) {
			continue
		}
		dest := fmt.Sprintf("%s/%d/", md.tigrisStatics.root, staticNum)
		staticNum += 1

		err = md.tigrisStatics.uploadDirectory(ctx, dest, path.Clean(static.GuestPath))
		if err != nil {
			return err
		}

		// TODO(allison): This is a temporary workaround.
		//                When they're available, we want to swap over to virtual services.
		md.appConfig.Statics = append(md.appConfig.Statics, appconfig.Static{
			GuestPath:     "/" + dest,
			UrlPrefix:     static.UrlPrefix,
			TigrisBucket:  md.tigrisStatics.bucket,
			IndexDocument: static.IndexDocument,
		})
	}

	return nil
}

// Delete old statics from the tigris bucket.
func (md *machineDeployment) staticsFinalize(ctx context.Context) error {

	io := iostreams.FromContext(ctx)

	// Delete old statics from the bucket.
	err := md.tigrisStatics.deleteOldStatics(ctx, md.appConfig.AppName, md.releaseVersion)
	if err != nil {
		fmt.Fprintf(io.ErrOut, "Failed to delete old statics: %v\n", err)
	}

	// TODO(allison): do we need to do anything else here? i.e. push new service config?
	//                this is dependent on the proxy work to support statics, which I don't
	//                *believe* is done yet.
	//                I presume configuring this would happen after machine deployment,
	//                since you should hypothetically be able to run a static site
	//                off of tigris and zero machines. we'll see :)
	return nil
}

// We failed, let's delete the incomplete push.
func (md *machineDeployment) staticsCleanupAfterFailure() {

	terminal.Debugf("Cleaning up failed statics push\n")

	deleteCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := md.tigrisStatics.deleteDirectory(deleteCtx, md.tigrisStatics.root)
	if err != nil {
		terminal.Debugf("Failed to delete statics: %v\n", err)
	}
}
