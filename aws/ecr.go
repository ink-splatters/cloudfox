package aws

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BishopFox/cloudfox/aws/sdk"
	"github.com/BishopFox/cloudfox/internal"
	"github.com/BishopFox/cloudfox/internal/aws/policy"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/bishopfox/awsservicemap"
	"github.com/sirupsen/logrus"
)

type ECRModule struct {
	// General configuration data
	ECRClient    sdk.AWSECRClientInterface
	Caller       sts.GetCallerIdentityOutput
	AWSRegions   []string
	OutputFormat string
	Goroutines   int
	AWSProfile   string
	WrapTable    bool

	// Main module data
	Repositories   []Repository
	CommandCounter internal.CommandCounter
	// Used to store output data for pretty printing
	output internal.OutputData2
	modLog *logrus.Entry
}

type Repository struct {
	AWSService string
	Region     string
	Name       string
	URI        string
	PushedAt   string
	ImageTags  string
	ImageSize  int64
	Policy     policy.Policy
	PolicyJSON string
}

func (m *ECRModule) PrintECR(outputFormat string, outputDirectory string, verbosity int) {
	// These stuct values are used by the output module
	m.output.Verbosity = verbosity
	m.output.Directory = outputDirectory
	m.output.CallingModule = "ecr"
	m.modLog = internal.TxtLog.WithFields(logrus.Fields{
		"module": m.output.CallingModule,
	})
	if m.AWSProfile == "" {
		m.AWSProfile = internal.BuildAWSPath(m.Caller)
	}

	fmt.Printf("[%s][%s] Enumerating container repositories for account %s.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), aws.ToString(m.Caller.Account))

	wg := new(sync.WaitGroup)
	semaphore := make(chan struct{}, m.Goroutines)

	// Create a channel to signal the spinner aka task status goroutine to finish
	spinnerDone := make(chan bool)
	//fire up the the task status spinner/updated
	go internal.SpinUntil(m.output.CallingModule, &m.CommandCounter, spinnerDone, "regions")

	//create a channel to receive the objects
	dataReceiver := make(chan Repository)

	// Create a channel to signal to stop
	receiverDone := make(chan bool)

	go m.Receiver(dataReceiver, receiverDone)

	for _, region := range m.AWSRegions {
		wg.Add(1)
		m.CommandCounter.Pending++
		go m.executeChecks(region, wg, semaphore, dataReceiver)

	}

	wg.Wait()
	//time.Sleep(time.Second * 2)

	// Send a message to the spinner goroutine to close the channel and stop
	spinnerDone <- true
	<-spinnerDone
	receiverDone <- true
	<-receiverDone

	// add - if struct is not empty do this. otherwise, dont write anything.
	m.output.Headers = []string{
		"Service",
		"Region",
		"Name",
		"URI",
		"PushedAt",
		"ImageTags",
		"ImageSize",
	}

	// Table rows
	for i := range m.Repositories {
		m.output.Body = append(
			m.output.Body,
			[]string{
				m.Repositories[i].AWSService,
				m.Repositories[i].Region,
				m.Repositories[i].Name,
				m.Repositories[i].URI,
				m.Repositories[i].PushedAt,
				m.Repositories[i].ImageTags,
				strconv.Itoa(int(m.Repositories[i].ImageSize)),
			},
		)

	}
	if len(m.output.Body) > 0 {
		m.output.FilePath = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", aws.ToString(m.Caller.Account), m.AWSProfile))
		//m.output.OutputSelector(outputFormat)
		//utils.OutputSelector(verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule)
		//internal.OutputSelector(verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule, m.WrapTable, m.AWSProfile)
		//m.writeLoot(m.output.FilePath, verbosity)
		o := internal.OutputClient{
			Verbosity:     verbosity,
			CallingModule: m.output.CallingModule,
			Table: internal.TableClient{
				Wrap: m.WrapTable,
			},
		}
		o.Table.TableFiles = append(o.Table.TableFiles, internal.TableFile{
			Header: m.output.Headers,
			Body:   m.output.Body,
			Name:   m.output.CallingModule,
		})
		o.PrefixIdentifier = m.AWSProfile
		o.Table.DirectoryName = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", aws.ToString(m.Caller.Account), m.AWSProfile))
		o.WriteFullOutput(o.Table.TableFiles, nil)
		m.writeLoot(o.Table.DirectoryName, verbosity)
		fmt.Printf("[%s][%s] %s repositories found.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), strconv.Itoa(len(m.output.Body)))
	} else {
		fmt.Printf("[%s][%s] No repositories found, skipping the creation of an output file.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile))
	}
	fmt.Printf("[%s][%s] For context and next steps: https://github.com/BishopFox/cloudfox/wiki/AWS-Commands#%s\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), m.output.CallingModule)
}

func (m *ECRModule) executeChecks(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Repository) {
	defer wg.Done()

	servicemap := &awsservicemap.AwsServiceMap{
		JsonFileSource: "DOWNLOAD_FROM_AWS",
	}
	res, err := servicemap.IsServiceInRegion("ecr", r)
	if err != nil {
		m.modLog.Error(err)
	}
	if res {
		m.CommandCounter.Total++
		wg.Add(1)
		m.getECRRecordsPerRegion(r, wg, semaphore, dataReceiver)
	}
}

func (m *ECRModule) Receiver(receiver chan Repository, receiverDone chan bool) {
	defer close(receiverDone)
	for {
		select {
		case data := <-receiver:
			m.Repositories = append(m.Repositories, data)
		case <-receiverDone:
			receiverDone <- true
			return
		}
	}
}

func (m *ECRModule) writeLoot(outputDirectory string, verbosity int) {
	path := filepath.Join(outputDirectory, "loot")
	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
	}
	pullFile := filepath.Join(path, "ecr-pull-commands.txt")

	var out string
	out = out + fmt.Sprintln("#############################################")
	out = out + fmt.Sprintln("# The profile you will use to perform these commands is most likely not the profile you used to run CloudFox")
	out = out + fmt.Sprintln("# Set the $profile environment variable to the profile you are going to use to inspect the repositories.")
	out = out + fmt.Sprintln("# E.g., export profile=dev-prod.")
	out = out + fmt.Sprintln("#############################################")
	out = out + fmt.Sprintln("")

	for _, repo := range m.Repositories {
		loginURI := strings.Split(repo.URI, "/")[0]
		out = out + fmt.Sprintf("aws --profile $profile --region %s ecr get-login-password | docker login --username AWS --password-stdin %s\n", repo.Region, loginURI)
		out = out + fmt.Sprintf("docker pull %s\n", repo.URI)
		out = out + fmt.Sprintf("docker inspect %s\n", repo.URI)
		out = out + fmt.Sprintf("docker history --no-trunc %s\n", repo.URI)
		out = out + fmt.Sprintf("docker run -it --entrypoint /bin/sh %s\n", repo.URI)
		out = out + fmt.Sprintf("docker save %s -o %s.tar\n\n", repo.URI, repo.Name)

	}
	err = os.WriteFile(pullFile, []byte(out), 0644)
	if err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
	}

	if verbosity > 2 {
		fmt.Println()
		fmt.Printf("[%s][%s] %s \n", cyan(m.output.CallingModule), cyan(m.AWSProfile), green("Use the commands below to authenticate to ECR and download the images that look interesting"))
		fmt.Printf("[%s][%s] %s \n\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), green("You will need the ecr:GetAuthorizationToken on the registry to authenticate and this is not part of the SecurityAudit permissions policy"))

		fmt.Print(out)
		fmt.Printf("[%s][%s] %s \n\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), green("End of loot file."))
	}

	fmt.Printf("[%s][%s] Loot written to [%s]\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), pullFile)

}

func (m *ECRModule) getECRRecordsPerRegion(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Repository) {
	defer func() {
		m.CommandCounter.Executing--
		m.CommandCounter.Complete++
		wg.Done()

	}()
	semaphore <- struct{}{}
	defer func() {
		<-semaphore
	}()

	var allImages []types.ImageDetail
	var repoURI string
	var repoName string

	DescribeRepositories, err := m.describeRepositories(r)
	if err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
		return
	}

	for _, repo := range DescribeRepositories {
		repoName = aws.ToString(repo.RepositoryName)
		repoURI = aws.ToString(repo.RepositoryUri)
		//created := *repo.CreatedAt
		//fmt.Printf("%s, %s, %s", repoName, repoURI, created)

		images, err := m.describeImages(r, repoName)
		if err != nil {
			m.modLog.Error(err.Error())
			m.CommandCounter.Error++
			return
		}
		allImages = append(allImages, images...)
	}

	sort.Slice(allImages, func(i, j int) bool {
		return allImages[i].ImagePushedAt.Format("2006-01-02 15:04:05") < allImages[j].ImagePushedAt.Format("2006-01-02 15:04:05")
	})

	var image types.ImageDetail
	var imageTags string

	if len(allImages) > 1 {
		image = allImages[len(allImages)-1]
	} else if len(allImages) == 1 {
		image = allImages[0]
	} else {
		return
	}

	if len(image.ImageTags) > 0 {
		imageTags = image.ImageTags[0]
	}
	//imageTags := image.ImageTags[0]
	pushedAt := image.ImagePushedAt.Format("2006-01-02 15:04:05")
	imageSize := aws.ToInt64(image.ImageSizeInBytes)
	pullURI := fmt.Sprintf("%s:%s", repoURI, imageTags)

	dataReceiver <- Repository{
		AWSService: "ECR",
		Name:       repoName,
		Region:     r,
		URI:        pullURI,
		PushedAt:   pushedAt,
		ImageTags:  imageTags,
		ImageSize:  imageSize,
	}

}

func (m *ECRModule) describeRepositories(r string) ([]types.Repository, error) {

	var repositories []types.Repository
	Repositories, err := sdk.CachedECRDescribeRepositories(m.ECRClient, aws.ToString(m.Caller.Account), r)
	if err != nil {
		m.CommandCounter.Error++
		return nil, err
	}

	repositories = append(repositories, Repositories...)

	return repositories, nil
}

func (m *ECRModule) describeImages(r string, repoName string) ([]types.ImageDetail, error) {
	var images []types.ImageDetail

	ImageDetails, err := sdk.CachedECRDescribeImages(m.ECRClient, aws.ToString(m.Caller.Account), r, repoName)
	if err != nil {
		m.CommandCounter.Error++
		return nil, err
	}
	images = append(images, ImageDetails...)
	return images, nil
}

func (m *ECRModule) getECRRepositoryPolicy(r string, repository string) (policy.Policy, error) {
	var repoPolicy policy.Policy
	Policy, err := sdk.CachedECRGetRepositoryPolicy(m.ECRClient, aws.ToString(m.Caller.Account), r, repository)
	if err != nil {
		m.CommandCounter.Error++
		return repoPolicy, err
	}
	repoPolicy, err = policy.ParseJSONPolicy([]byte(Policy))
	if err != nil {
		return repoPolicy, fmt.Errorf("parsing policy (%s) as JSON: %s", repository, err)
	}
	return repoPolicy, nil
}
