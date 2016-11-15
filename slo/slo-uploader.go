package slo

import (
	"fmt"
	"github.com/ibmjstart/swiftlygo/auth"
	"io"
	"os"
	"time"
)

// maxFileChunks is the maximum number of chunks that OpenStack Object
// storage allows within an SLO.
const maxFileChunks uint = 1000

// maxChunkSize is the largest allowable size for a single chunk in
// OpenStack object storage.
const maxChunkSize uint = 1000 * 1000 * 1000 * 5

// Uploader uploads a file to object storage
type Uploader struct {
	outputChannel  chan string
	Status         *Status
	source         io.ReaderAt
	connection     auth.Destination
	pipelineSource <-chan FileChunk
	pipeline       chan FileChunk
	uploadCounts   <-chan Count
	errors         chan error
	maxUploaders   uint
}

func getSize(file *os.File) (uint, error) {
	dataStats, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("Failed to get stats about local data file %s: %s", file.Name(), err)
	}
	return uint(dataStats.Size()), nil
}

func NewUploader(connection auth.Destination, chunkSize uint, container string,
	object string, source *os.File, maxUploads uint, onlyMissing bool, outputFile io.Writer) (*Uploader, error) {
	var (
		serversideChunks []string
		err              error
	)
	if source == nil {
		return nil, fmt.Errorf("Unable to upload nil file")
	}

	if maxUploads < 1 {
		return nil, fmt.Errorf("Unable to upload with %d uploaders (minimum 1 required)", maxUploads)
	}
	outputChannel := make(chan string, 10)

	if container == "" {
		return nil, fmt.Errorf("Container name cannot be the emtpy string")
	} else if object == "" {
		return nil, fmt.Errorf("Object name cannot be the emtpy string")
	}

	if chunkSize > maxChunkSize || chunkSize < 1 {
		return nil, fmt.Errorf("Chunk size must be between 1byte and 5GB")
	}

	// Define a function that prints manifest names when the pass through
	printManifest := func(chunk FileChunk) (FileChunk, error) {
		outputChannel <- fmt.Sprintf("Uploading manifest: %s\n", chunk.Path())
		return chunk, nil
	}

	// set up the list of missing chunks
	if onlyMissing {
		serversideChunks, err = connection.FileNames(container)
		if err != nil {
			outputChannel <- fmt.Sprintf("Problem getting existing chunks names from object storage: %s\n", err)
		}
	} else {
		serversideChunks = make([]string, 0)
	}

	// Asynchronously print everything that comes in on this channel
	go func(output io.Writer, incoming chan string) {
		for message := range incoming {
			_, err := fmt.Fprintln(output, message)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error writing to output log: %s\n", err)
			}
		}
	}(outputFile, outputChannel)

	fileSize, err := getSize(source)
	if err != nil {
		return nil, err
	}
	// construct pipeline data source
	fromSource, numberChunks := BuildChunks(uint(fileSize), chunkSize)

	// start status
	status := newStatus(numberChunks, chunkSize, outputChannel)
	// Asynchronously print status every 5 seconds
	go func(status *Status, intervalSeconds uint) {
		for {
			time.Sleep(time.Duration(intervalSeconds) * time.Second)
			status.print()
		}
	}(status, 60)

	// Initialize pipeline, but don't pass in data
	intoPipeline := make(chan FileChunk)
	errors := make(chan error, 100)
	chunks := ObjectNamer(intoPipeline, object+"-chunk-%04[1]d-size-%[2]d")
	chunks = Containerizer(chunks, container)
	// Read data for chunks
	chunks = ReadData(chunks, errors, source)
	// Perform the hash
	chunks = HashData(chunks, errors)
	// Perform upload
	// Separate out chunks that should not be uploaded
	noupload, chunks := Separate(chunks, errors, func(chunk FileChunk) (bool, error) {
		for _, chunkName := range serversideChunks {
			if chunkName == chunk.Object {
				return true, nil
			}
		}
		return false, nil
	})
	uploadStreams := Divide(chunks, maxUploads)
	doneStreams := make([]<-chan FileChunk, maxUploads)
	for index, stream := range uploadStreams {
		doneStreams[index] = UploadData(stream, errors, connection, time.Second)
	}
	chunks = Join(doneStreams...)
	// Join stream of chunks back together
	chunks = Join(noupload, chunks)
	chunks = Map(chunks, errors, func(chunk FileChunk) (FileChunk, error) {
		chunk.Data = nil // Discard data to allow it to be garbage-collected
		return chunk, nil
	})
	chunks, uploadCounts := Counter(chunks)

	// Build manifest layer 1
	manifests := ManifestBuilder(chunks, errors)
	manifests = ObjectNamer(manifests, object+"-manifest-%04[1]d")
	manifests = Containerizer(manifests, container)
	// Upload manifest layer 1
	manifests = Map(manifests, errors, printManifest)
	manifests = UploadManifests(manifests, errors, connection)
	// Build top-level manifest out of layer 1
	topManifests := ManifestBuilder(manifests, errors)
	topManifests = ObjectNamer(topManifests, object)
	topManifests = Containerizer(topManifests, container)
	// Upload top-level manifest
	topManifests = Map(topManifests, errors, printManifest)
	topManifests = UploadManifests(topManifests, errors, connection)

	// close the errors channel after topManifests is empty
	go func() {
		defer close(errors)
		for _ = range topManifests {
		}
	}()
	return &Uploader{
		outputChannel:  outputChannel,
		Status:         status,
		connection:     connection,
		source:         source,
		pipeline:       intoPipeline,
		pipelineSource: fromSource,
		uploadCounts:   uploadCounts,
		errors:         errors,
		maxUploaders:   maxUploads,
	}, nil
}

// Upload uploads the sloUploader's source file to object storage
func (u *Uploader) Upload() error {
	u.Status.start()
	// drain the upload counts
	go func() {
		defer u.Status.stop()
		for _ = range u.uploadCounts {
			u.Status.uploadComplete()
		}
	}()

	// start sending chunks through the pipeline.
	for chunk := range u.pipelineSource {
		u.pipeline <- chunk
	}
	// Drain the errors channel, this will block until the errors channel is closed above.
	for e := range u.errors {
		u.outputChannel <- e.Error()
	}
	return nil
}
