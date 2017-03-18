package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"regexp"
)

type docIndex struct {
	DocID  string
	Ngrams map[int32][]indexedNgram
}

type indexedNgram []int32

type ngramMatch struct {
	source indexedNgram
	target indexedNgram
	ngram  int32
}

type matchingParams struct {
	matchingWindowSize            int32
	maxGap                        int32
	minimumMatchingNgrams         int32
	minimumMatchingNgramsInWindow int32
	commonNgramsLimit             float32
	minimumMatchingNgramsInDocs   int32
	contextSize                   int32
	banalNgrams                   int
	mergeOnByteDistance           bool
	passageDistanceMultiplier     float32
	twoWayMatching                bool
	batchSize                     int
	outputPath                    string
	numThreads                    int
	outputFormat                  string
	sortingField                  string
}

type matchValues struct {
	inAlignment               bool
	matchesInCurrentAlignment int32
	matchesInCurrentWindow    int32
	sourceAnchor              int32
	lastSourcePosition        int32
	targetAnchor              int32
	lastTargetPosition        int32
	previousSourceIndex       int32
	commonNgramMatches        int32
	maxSourceGap              int32
	maxTargetGap              int32
	sourceWindowBoundary      int32
	targetWindowBoundary      int32
	currentAlignment          Alignment
	previousAlignment         Alignment
	firstMatch                []indexedNgram
	lastMatch                 []indexedNgram
}

// Alignment is the matching representation
type Alignment struct {
	source position
	target position
}

type position struct {
	startByte int32
	endByte   int32
}

type alignmentsPerDoc struct {
	docID   string
	matches []Alignment
}

// CombinedAlignments holds all alignments for a single source doc
type CombinedAlignments struct {
	sourceID   string
	alignments []alignmentsPerDoc
}

// Pair is a data structure to hold a key/value pair.
type Pair struct {
	Key   int32
	Value int
}

// PairList is a slice of Pairs that implements sort. Interface to sort by Value.
type PairList []Pair

func (p PairList) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p PairList) Len() int           { return len(p) }
func (p PairList) Less(i, j int) bool { return p[i].Value > p[j].Value }

// A function to turn a map into a PairList, then sort and return it.
func sortMapByValue(m map[int32]int) PairList {
	p := make(PairList, len(m))
	i := 0
	for k, v := range m {
		p[i] = Pair{k, v}
	}
	sort.Sort(p)
	return p
}

var tags = regexp.MustCompile("<[^>]*?>")
var brokenBeginTags = regexp.MustCompile("^[^<]*?>")
var brokenEndTags = regexp.MustCompile("<[^>]*?$")
var spaces = regexp.MustCompile(" +")

func checkErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func parseFlags() ([]string, []string, map[string]map[string]string, map[string]map[string]string, map[int32]bool, matchingParams) {
	sourceFilesArg := flag.String("source_files", "", "source files location")
	targetFilesArg := flag.String("target_files", "", "target files location")
	threadsArg := flag.Int("threads", 4, "number of threads to use")
	sourceMetadataArg := flag.String("source_metadata", "", "path to source metadata")
	targetMetadataArg := flag.String("target_metadata", "", "path to target metadata")
	sourceCommonNgramsArg := flag.String("source_common_ngrams", "", "path to a JSON list of the most common ngrams in source files")
	targetCommonNgramsArg := flag.String("target_common_ngrams", "", "path to a JSON list of the most common ngrams in target files")
	mostCommonNgramThreshold := flag.Int("most_common_ngram_threshold", 1000, "take the n most common ngrams from source and target common ngrams")
	outputPath := flag.String("output_path", "./", "output path for results")
	outputFormat := flag.String("output_format", "tab", "output format of results")
	sortField := flag.String("sort_by", "year", "metadata field used to sort files in ascending order")
	batchSize := flag.Int("batch_size", 1, "batch steps defines the number of steps in which the full source vs target is run: useful when RAM usage is a concern")
	matchingWindowSize := int32(*flag.Int("matching_window_size", 20, "size of sliding window for matches"))
	maxGap := int32(*flag.Int("max_gap", 10, "maximum gap between two matching ngrams"))
	minimumMatchingNgrams := int32(*flag.Int("minimum_matching_ngrams", 4, "minimum matching ngrams to constitue a match"))
	minimumMatchingNgramsInWindow := int32(*flag.Int("minimum_matching_ngram_in_window", 4, "minimum matching ngrams per sliding window"))
	commonNgramsLimit := float32(*flag.Int("common_ngrams_limit", 75, "percentage of common ngrams to dismiss a match as banal")) / 100
	minimumMatchingNgramsInDocs := int32(*flag.Int("minimum_matching_ngrams_in_docs", 4, "minimum unique ngrams matching between docs to start comparison"))
	contextSize := int32(*flag.Int("context_size", 300, "size of context for before and after matching passages"))
	banalNgrams := *flag.Int("banal_ngrams", 25, "The top banal ngrams between two docs: used to define common, or banal ngrams")
	mergeOnByteDistance := *flag.Bool("merge_passages_on_byte_distance", true, "Merge passages within x number of byte: number defined by passage length and the passage_distance_multiplier option")
	passageDistance := float32(*flag.Float64("passage_distance_multiplier", 0.05, "Combine passage which are within (multiplier*length of previous passage) bytes"))
	twoWayMatching := *flag.Bool("two_way_matching", false, "Enable two way matching: source is compared to target and then target compared to source")
	flag.Parse()
	config := matchingParams{matchingWindowSize, maxGap, minimumMatchingNgrams, minimumMatchingNgramsInWindow, commonNgramsLimit, minimumMatchingNgramsInDocs,
		contextSize, banalNgrams, mergeOnByteDistance, passageDistance, twoWayMatching, *batchSize, *outputPath, *threadsArg, *outputFormat, *sortField}
	fmt.Println("Loading bibliography...")
	sourceMetadata := openJSONMetadata(sourceMetadataArg)
	targetMetadata := openJSONMetadata(targetMetadataArg)
	sourceFiles := getFiles(*sourceFilesArg, sourceMetadata, *sortField)
	targetFiles := getFiles(*targetFilesArg, targetMetadata, *sortField)
	mostCommonNgrams := compileMostCommonNgrams(sourceCommonNgramsArg, targetCommonNgramsArg, mostCommonNgramThreshold)
	return sourceFiles, targetFiles, sourceMetadata, targetMetadata, mostCommonNgrams, config
}

func openJSONMetadata(fileLocation *string) map[string]map[string]string {
	if *fileLocation == "" {
		return map[string]map[string]string{}
	}
	var filePath string
	if !strings.HasPrefix(*fileLocation, "/") {
		filePath = "./" + *fileLocation
	} else {
		filePath = *fileLocation
	}
	jsonFile, err := ioutil.ReadFile(filePath)
	checkErr(err)
	metadata := make(map[string]map[string]string)
	json.Unmarshal(jsonFile, &metadata)
	return metadata
}

func getFiles(filePath string, metadata map[string]map[string]string, sortField string) []string {
	if filePath == "" {
		return []string{}
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "./" + filePath
	}
	directory, err := os.Open(filePath)
	checkErr(err)
	files, err := directory.Readdir(-1)
	checkErr(err)
	var filesToLoad []string
	for _, fileInfo := range files {
		if !fileInfo.IsDir() {
			file := filepath.Join(filePath, fileInfo.Name())
			filesToLoad = append(filesToLoad, file)
		}
	}
	sortFieldIsNumeric := false
	for _, fields := range metadata {
		if _, ok := fields[sortField]; !ok {
			sortField = ""
			break
		}
		if _, err := strconv.Atoi(fields[sortField]); err == nil {
			sortFieldIsNumeric = true
		}
		break
	}
	if sortField != "" {
		sort.Slice(filesToLoad, func(i, j int) bool {
			first := path.Base(strings.Replace(filesToLoad[i], ".json", "", 1))
			second := path.Base(strings.Replace(filesToLoad[j], ".json", "", 1))
			if sortFieldIsNumeric {
				firstInt, err := strconv.Atoi(metadata[first][sortField])
				if err != nil {
					return false
				}
				secondInt, err := strconv.Atoi(metadata[second][sortField])
				if err != nil {
					return true
				}
				if firstInt < secondInt {
					return true
				} else if firstInt > secondInt {
					return false
				}
				return first < second
			}
			return metadata[first][sortField] < metadata[second][sortField]
		})
	} else {
		sort.Slice(filesToLoad, func(i, j int) bool {
			first, _ := strconv.Atoi(path.Base(strings.Replace(filesToLoad[i], ".json", "", 1)))
			second, _ := strconv.Atoi(path.Base(strings.Replace(filesToLoad[j], ".json", "", 1)))
			return first < second
		})
	}
	return filesToLoad
}

func compileMostCommonNgrams(sourceNgrams *string, targetNgrams *string, mostCommonNgramThreshold *int) map[int32]bool {
	mostCommonNgrams := []string{}
	if *sourceNgrams != "" {
		jsonFile, err := ioutil.ReadFile(*sourceNgrams)
		checkErr(err)
		json.Unmarshal(jsonFile, &mostCommonNgrams)
	}
	if len(mostCommonNgrams) > 0 {
		mostCommonNgrams = mostCommonNgrams[:*mostCommonNgramThreshold]
	}
	if *targetNgrams != "" {
		jsonFile, err := ioutil.ReadFile(*targetNgrams)
		checkErr(err)
		targetCommonNgrams := []string{}
		json.Unmarshal(jsonFile, &targetCommonNgrams)
		mostCommonNgrams = append(mostCommonNgrams, targetCommonNgrams[:*mostCommonNgramThreshold]...)
	}
	uniqueNgrams := make(map[int32]bool)
	for _, ngram := range mostCommonNgrams {
		ngramInt, _ := strconv.Atoi(ngram)
		uniqueNgrams[int32(ngramInt)] = true
	}
	return uniqueNgrams
}

func getJSONDocs(fileLocations []string) []docIndex {
	var jsonFiles []docIndex
	for _, fileLocation := range fileLocations {
		jsonFile, err := ioutil.ReadFile(fileLocation)
		checkErr(err)
		doc := make(map[int32][]indexedNgram)
		json.Unmarshal(jsonFile, &doc)
		docID := path.Base(strings.Replace(fileLocation, ".json", "", 1))
		docObject := docIndex{docID, doc}
		jsonFiles = append(jsonFiles, docObject)
	}
	return jsonFiles
}

func getIntersection(sourceFile *docIndex, targetFile *docIndex) map[int32]int {
	intersectCount := make(map[int32]int)
	if len(sourceFile.Ngrams) < len(targetFile.Ngrams) {
		for ngram := range sourceFile.Ngrams {
			if _, ok := targetFile.Ngrams[ngram]; ok {
				intersectCount[ngram] = len(sourceFile.Ngrams[ngram]) + len(targetFile.Ngrams[ngram])
			}
		}
	} else {
		for ngram := range targetFile.Ngrams {
			if _, ok := sourceFile.Ngrams[ngram]; ok {
				intersectCount[ngram] = len(sourceFile.Ngrams[ngram]) + len(targetFile.Ngrams[ngram])
			}
		}
	}
	return intersectCount
}

func getMostCommonNgrams(intersectionCount map[int32]int, banalNgrams *int, commonNgrams map[int32]bool) map[int32]bool {
	sortedIntersection := sortMapByValue(intersectionCount)
	mostCommonNgrams := make(map[int32]bool, len(commonNgrams))
	var count int
	for _, pair := range sortedIntersection {
		if pair.Value == 2 {
			break
		}
		mostCommonNgrams[pair.Key] = true
		count++
		if count == *banalNgrams {
			break
		}
	}
	for commonNgrams := range commonNgrams {
		mostCommonNgrams[commonNgrams] = true
	}
	return mostCommonNgrams
}

func createOutputFile(config *matchingParams, sourceMetadata map[string]map[string]string, targetMetadata map[string]map[string]string) (*os.File, []string, []string) {
	os.MkdirAll(config.outputPath, 0755)
	mergedOutput, err := os.Create(fmt.Sprintf(filepath.Join(config.outputPath, "alignments_results.%s"), config.outputFormat))
	checkErr(err)
	var firstSourceKey string
	for sourceKey := range sourceMetadata {
		firstSourceKey = sourceKey
		break
	}
	firstRow := []string{"source_doc_id"}
	sourceFields := mapToSliceOfKeys(sourceMetadata[firstSourceKey])
	firstRow = append(firstRow, sourceFields...)
	firstRow = append(firstRow, []string{"source_start_byte", "source_end_byte"}...)
	firstRow = append(firstRow, []string{"source_context_before", "source_passage", "source_context_after"}...)
	var firstTargetKey string
	for targetKey := range targetMetadata {
		firstTargetKey = targetKey
		break
	}
	firstRow = append(firstRow, "target_doc_id")
	targetFields := mapToSliceOfKeys(targetMetadata[firstTargetKey])
	firstRow = append(firstRow, targetFields...)
	firstRow = append(firstRow, []string{"target_start_byte", "target_end_byte"}...)
	firstRow = append(firstRow, []string{"target_context_before", "target_passage", "target_context_after"}...)
	if config.outputFormat == "tab" {
		mergedOutput.WriteString(strings.Join(firstRow, "\t") + "\n")
	} else {
		mergedOutput.WriteString("<html>\n")
	}
	return mergedOutput, sourceFields, targetFields
}

func makeSliceOfSlices(sliceToSlice []string, config *matchingParams) [][]string {
	var sliceOfSlices [][]string
	sliceLength := len(sliceToSlice)
	chunkSize := (sliceLength + config.batchSize - 1) / config.batchSize
	for i := 0; i < sliceLength; i += chunkSize {
		end := i + chunkSize
		if end > sliceLength {
			end = sliceLength
		}
		sliceOfSlices = append(sliceOfSlices, sliceToSlice[i:end])
	}
	return sliceOfSlices
}

func writeAligments(combinedAlignments *CombinedAlignments, sourceDocID *string, sourceMetadata map[string]map[string]string,
	targetMetadata map[string]map[string]string, f *os.File, config *matchingParams, sourceFields []string, targetFields []string) {
	var combinedOutput []string
	sourceValues := mapToSliceOfValues(sourceMetadata[*sourceDocID], sourceFields)
	for _, alignments := range combinedAlignments.alignments {
		targetValues := mapToSliceOfValues(targetMetadata[alignments.docID], targetFields)
		for _, alignment := range alignments.matches {
			var fields string
			if config.outputFormat == "tab" {
				fields = tabOutput(alignment, sourceDocID, alignments.docID, sourceMetadata, targetMetadata, sourceValues, targetValues, config)

			} else {
				fields = htmlOutput(alignment, sourceDocID, alignments.docID, sourceMetadata, targetMetadata, sourceValues, targetValues, config)
			}
			combinedOutput = append(combinedOutput, fields)
		}
	}
	if config.outputFormat == "tab" {
		f.WriteString("\n" + strings.Join(combinedOutput, "\n"))
	} else {
		f.WriteString("\n" + strings.Join(combinedOutput, ""))
	}
}

func tabOutput(alignment Alignment, sourceDocID *string, targetDocID string, sourceMetadata map[string]map[string]string,
	targetMetadata map[string]map[string]string, sourceValues []string, targetValues []string, config *matchingParams) string {
	fields := []string{*sourceDocID}
	fields = append(fields, sourceValues...)
	fields = append(fields, []string{strconv.Itoa(int(alignment.source.startByte)), strconv.Itoa(int(alignment.source.endByte))}...)
	fields = append(fields, alignmentToText(&alignment.source, sourceMetadata[*sourceDocID]["filename"], config)...)
	fields = append(fields, targetDocID)
	fields = append(fields, targetValues...)
	fields = append(fields, []string{strconv.Itoa(int(alignment.target.startByte)), strconv.Itoa(int(alignment.target.endByte))}...)
	fields = append(fields, alignmentToText(&alignment.target, targetMetadata[targetDocID]["filename"], config)...)
	return strings.Join(fields, "\t")
}

func htmlOutput(alignment Alignment, sourceDocID *string, targetDocID string, sourceMetadata map[string]map[string]string,
	targetMetadata map[string]map[string]string, sourceValues []string, targetValues []string, config *matchingParams) string {
	outputString := ""
	outputString += "<h1>===================</h1>"
	outputString += "<div><button type='button'>Diff alignments</button>"
	sourcePassages := alignmentToText(&alignment.source, sourceMetadata[*sourceDocID]["filename"], config)
	targetPassages := alignmentToText(&alignment.target, targetMetadata[targetDocID]["filename"], config)
	outputString += fmt.Sprintf("<h4>====== Source ======</h4><h5>%s, (%s)</h5>", sourceMetadata[*sourceDocID]["title"], sourceMetadata[*sourceDocID]["author"])
	outputString += fmt.Sprintf("<p>%s <span style='color:red'>%s</span> %s</p>", sourcePassages[0], sourcePassages[1], sourcePassages[2])
	outputString += fmt.Sprintf("<h4>====== Target ======</h4><h5>%s, (%s)</h5>", targetMetadata[targetDocID]["title"], targetMetadata[targetDocID]["author"])
	outputString += fmt.Sprintf("<p>%s <span style='color:red'>%s</span> %s</p>", targetPassages[0], targetPassages[1], targetPassages[2])
	outputString += "</div>"
	return outputString
}

func mapToSliceOfKeys(metadata map[string]string) []string {
	keys := []string{}
	for k := range metadata {
		keys = append(keys, k)
	}
	return keys
}

func mapToSliceOfValues(metadata map[string]string, fields []string) []string {
	values := []string{}
	for _, v := range fields {
		values = append(values, metadata[v])
	}
	return values
}

func alignmentToText(alignment *position, filename string, config *matchingParams) []string {
	beforeContext := getText(&filename, alignment.startByte-int32(config.contextSize), alignment.startByte)
	matchingPassage := getText(&filename, alignment.startByte, alignment.endByte)
	afterContext := getText(&filename, alignment.endByte, alignment.endByte+int32(config.contextSize))
	passages := []string{beforeContext, matchingPassage, afterContext}
	return passages
}

func getText(fileLocation *string, startByte int32, endByte int32) string {
	f, err := os.Open(*fileLocation)
	checkErr(err)
	if startByte < 0 {
		startByte = int32(0)
	}
	_, err = f.Seek(int64(startByte), 0)
	checkErr(err)
	passage := make([]byte, endByte-startByte)
	_, err = f.Read(passage)
	checkErr(err)
	text := string(passage)
	text = tags.ReplaceAllString(text, "")
	text = brokenBeginTags.ReplaceAllString(text, "")
	text = brokenEndTags.ReplaceAllString(text, "")
	text = strings.Replace(text, "\t", " ", -1)
	text = strings.Replace(text, "\n", " ", -1)
	text = spaces.ReplaceAllString(text, " ")
	f.Close()
	return text
}

func addAlignment(m *matchValues, config *matchingParams, alignments *[]Alignment) {
	m.currentAlignment.source = position{m.firstMatch[0][1], m.lastMatch[0][2]}
	m.currentAlignment.target = position{m.firstMatch[1][1], m.lastMatch[1][2]}
	if config.mergeOnByteDistance && mergeWithPrevious(m, config, *alignments) {
		(*alignments)[len(*alignments)-1].source.endByte = m.currentAlignment.source.endByte
		(*alignments)[len(*alignments)-1].target.endByte = m.currentAlignment.target.endByte
	} else {
		*alignments = append(*alignments, m.currentAlignment)
		m.previousAlignment = m.currentAlignment
	}
}

func matchPassage(sourceFile *docIndex, targetFile *docIndex, matches []ngramMatch, config *matchingParams, mostCommonNgrams map[int32]bool) []Alignment {
	alignments := make([]Alignment, 0)
	m := matchValues{}
	m.lastSourcePosition = 0
	m.inAlignment = false
	for matchIndex, currentAnchor := range matches {
		if currentAnchor.source[0] < m.lastSourcePosition {
			continue
		}
		m.sourceAnchor = currentAnchor.source[0]
		m.sourceWindowBoundary = m.sourceAnchor + config.matchingWindowSize
		m.lastSourcePosition = m.sourceAnchor
		m.maxSourceGap = m.lastSourcePosition + config.maxGap
		m.targetAnchor = currentAnchor.target[0]
		m.targetWindowBoundary = m.targetAnchor + config.matchingWindowSize
		m.lastTargetPosition = m.targetAnchor
		m.maxTargetGap = m.lastTargetPosition + config.maxGap
		m.inAlignment = true
		m.previousSourceIndex = m.sourceAnchor
		m.firstMatch = []indexedNgram{currentAnchor.source, currentAnchor.target}
		m.matchesInCurrentAlignment = 1
		m.matchesInCurrentWindow = 1
		m.commonNgramMatches = 0
		if _, ok := mostCommonNgrams[currentAnchor.ngram]; ok {
			m.commonNgramMatches++
		}
		m.lastMatch = []indexedNgram{currentAnchor.source, currentAnchor.target}
	innerMatchingLoop:
		for _, match := range matches[matchIndex+1:] {
			source, target := match.source, match.target
			// we skip source_match if the same as before and we only want targets that are after last target match
			if source[0] == m.previousSourceIndex || target[0] <= m.lastTargetPosition {
				continue
			}
			if source[0] > m.maxSourceGap || target[0] > m.maxTargetGap {
				m.inAlignment = false
			}
			if source[0] > m.sourceWindowBoundary || target[0] > m.targetWindowBoundary {
				if m.matchesInCurrentWindow < config.minimumMatchingNgramsInWindow {
					m.inAlignment = false
				} else {
					if source[0] > m.maxSourceGap || target[0] > m.maxTargetGap {
						m.inAlignment = false
					} else {
						m.sourceAnchor = source[0]
						m.sourceWindowBoundary = m.sourceAnchor + config.matchingWindowSize
						m.targetAnchor = target[0]
						m.targetWindowBoundary = m.targetAnchor + config.matchingWindowSize
						m.matchesInCurrentWindow = 0
					}
				}
			}
			if !m.inAlignment {
				if float32(m.commonNgramMatches/m.matchesInCurrentAlignment) < config.commonNgramsLimit {
					if m.matchesInCurrentAlignment >= config.minimumMatchingNgramsInWindow {
						addAlignment(&m, config, &alignments)
						// Looking for small match within max_gap
					} else if (m.lastMatch[0][0]-currentAnchor.source[0]) <= config.maxGap && m.matchesInCurrentAlignment >= config.minimumMatchingNgrams {
						addAlignment(&m, config, &alignments)
					}
				}
				m.lastSourcePosition = m.lastMatch[0][0] + 1 // Make sure we start the next match at index that follows last source match
				break innerMatchingLoop
			}
			m.lastSourcePosition = source[0]
			m.maxSourceGap = m.lastSourcePosition + config.maxGap
			m.lastTargetPosition = target[0]
			m.maxTargetGap = m.lastTargetPosition + config.maxGap
			m.previousSourceIndex = source[0]
			m.matchesInCurrentWindow++
			m.matchesInCurrentAlignment++
			m.lastMatch = []indexedNgram{source, target} // save last matching ngrams
			if _, ok := mostCommonNgrams[match.ngram]; ok {
				m.commonNgramMatches++
			}
		}
	}
	// Add current alignment if not already done
	if m.inAlignment && m.matchesInCurrentAlignment >= config.minimumMatchingNgrams {
		addAlignment(&m, config, &alignments)
	}
	return alignments
}

func reverseMatch(sourceFile *docIndex, targetFile *docIndex, matches []ngramMatch, config *matchingParams, mostCommonNgrams map[int32]bool, alignments []Alignment) []Alignment {
	var invertMatches = []ngramMatch{}
	for _, match := range matches {
		invertMatches = append(invertMatches, ngramMatch{match.target, match.source, match.ngram})
	}
	sort.Slice(invertMatches, func(i, j int) bool {
		if invertMatches[i].source[0] < invertMatches[j].source[0] {
			return true
		} else if invertMatches[i].source[0] > invertMatches[j].source[0] {
			return false
		}
		return invertMatches[i].target[0] < invertMatches[j].target[0]
	})
	reverseAlignments := matchPassage(targetFile, sourceFile, invertMatches, config, mostCommonNgrams)
	targetMergeSet := make(map[int]bool)
	for sourceMatchIndex, sourceMatch := range alignments {
	innerTwoWay:
		for targetMatchIndex, targetMatch := range reverseAlignments {
			if _, ok := targetMergeSet[targetMatchIndex]; ok {
				continue
			}
			if targetMatch.target.startByte < sourceMatch.source.startByte && targetMatch.target.endByte >= sourceMatch.source.startByte && targetMatch.source.startByte < sourceMatch.target.startByte && targetMatch.source.endByte >= sourceMatch.target.startByte {
				if targetMatch.target.endByte > sourceMatch.source.endByte {
					// fmt.Println("1 Extended source from", sourceMatch.source, "to", targetMatch.target)
					alignments[sourceMatchIndex] = Alignment{targetMatch.target, targetMatch.source}
				} else {
					sourcePosition := position{targetMatch.target.startByte, sourceMatch.source.endByte}
					targetPosition := position{targetMatch.source.startByte, sourceMatch.target.endByte}
					alignments[sourceMatchIndex] = Alignment{sourcePosition, targetPosition}
					// fmt.Println("2 Extended source from", sourceMatch.source, sourceMatch.target, "to", Alignment{sourcePosition, targetPosition})
				}
				targetMergeSet[targetMatchIndex] = true
				break innerTwoWay
			} else if targetMatch.target.endByte > sourceMatch.source.endByte && targetMatch.target.startByte <= sourceMatch.source.endByte && targetMatch.source.endByte > sourceMatch.target.endByte && targetMatch.source.startByte <= sourceMatch.target.endByte {
				sourcePosition := position{sourceMatch.source.startByte, targetMatch.target.endByte}
				targetPosition := position{sourceMatch.target.startByte, targetMatch.source.endByte}
				alignments[sourceMatchIndex] = Alignment{sourcePosition, targetPosition}
				// fmt.Println("3 Extended source from", sourceMatch.source, sourceMatch.target, "to", Alignment{sourcePosition, targetPosition})
				targetMergeSet[targetMatchIndex] = true
				break innerTwoWay
			}
		}
	}
	for targetMatchIndex, targetMatch := range reverseAlignments {
		if _, ok := targetMergeSet[targetMatchIndex]; !ok {
			alignments = append(alignments, Alignment{targetMatch.target, targetMatch.source})
			// fmt.Println("Adding", targetMatch.target, targetMatch.source)
		}
	}
	sort.Slice(alignments, func(i, j int) bool {
		if alignments[i].source.startByte < alignments[j].source.startByte {
			return true
		} else if alignments[i].source.startByte > alignments[j].source.startByte {
			return false
		}
		return alignments[i].target.startByte < alignments[j].target.startByte
	})
	var newAlignments = []Alignment{}
	m := matchValues{}
	for pos, match := range alignments {
		if pos == 0 {
			m.previousAlignment = match
			continue
		}
		m.currentAlignment = match
		if mergeWithPrevious(&m, config, alignments) {
			m.previousAlignment.source.endByte = m.currentAlignment.source.endByte
			m.previousAlignment.target.endByte = m.currentAlignment.target.endByte
		} else {
			newAlignments = append(newAlignments, m.previousAlignment)
			m.previousAlignment = m.currentAlignment
			if pos == len(alignments)-1 {
				newAlignments = append(newAlignments, m.currentAlignment)
			}
		}
	}
	return newAlignments
}

func mergeWithPrevious(m *matchValues, config *matchingParams, alignments []Alignment) bool {
	distanceValue := int32((float32(m.previousAlignment.source.endByte - m.previousAlignment.source.startByte)) * config.passageDistanceMultiplier)
	maxSourceDistance := m.currentAlignment.source.startByte - distanceValue
	maxTargetDistance := m.currentAlignment.target.startByte - distanceValue
	// Merge passages that are within distanceValue measured in bytes
	if len(alignments) > 0 && m.previousAlignment.source.startByte <= maxSourceDistance && maxSourceDistance <= m.previousAlignment.source.endByte && m.previousAlignment.target.startByte <= maxTargetDistance && maxTargetDistance <= m.previousAlignment.target.endByte {
		return true
	}
	return false
}

func main() {
	sourceFiles, targetFiles, sourceMetadata, targetMetadata, commonNgrams, config := parseFlags()
	sourceAgainstSource := false
	sourceFilesDone := make(map[string]bool)

	// Split source and target files into config.batchSize batches
	sourceFileBatches := makeSliceOfSlices(sourceFiles, &config)
	var targetFileBatches [][]string
	if len(targetFiles) == 0 {
		targetMetadata = sourceMetadata
		sourceAgainstSource = true
		targetFileBatches = sourceFileBatches
	} else {
		targetFileBatches = makeSliceOfSlices(targetFiles, &config)
	}

	mergedOutput, sourceFields, targetFields := createOutputFile(&config, sourceMetadata, targetMetadata)
	counts := 0
	for sourceBatchNumber := 0; sourceBatchNumber < config.batchSize; sourceBatchNumber++ {
		if config.batchSize > 1 {
			fmt.Printf("\n#### Loading source batch %d... ###\n", sourceBatchNumber+1)
		} else {
			fmt.Println("Loading source files...")
		}
		sourceFileIndexes := getJSONDocs(sourceFileBatches[sourceBatchNumber])
		for targetBatchNumber := 0; targetBatchNumber < config.batchSize; targetBatchNumber++ {
			if sourceAgainstSource && sourceBatchNumber > targetBatchNumber {
				continue // we've already done these comparisons in the other direction
			}
			var targetFileIndexes []docIndex
			if sourceAgainstSource && targetBatchNumber == sourceBatchNumber {
				targetFileIndexes = sourceFileIndexes
			} else {
				if config.batchSize > 1 {
					fmt.Printf("Loading target batch %d...\n", targetBatchNumber+1)
				}
				targetFileIndexes = getJSONDocs(targetFileBatches[targetBatchNumber])
			}
			var localSourceFilesDone map[string]bool
			if sourceAgainstSource {
				if len(sourceFilesDone) > 0 {
					localSourceFilesDone = sourceFilesDone
				} else {
					localSourceFilesDone = make(map[string]bool)
				}
			}
			for _, sourceFile := range sourceFileIndexes {
				if config.batchSize == 1 {
					fmt.Printf("Comparing source file %s to all...\n", sourceFile.DocID)
				}

				if config.batchSize > 1 {
					fmt.Printf("Comparing source file %s to target batch %d...\n", sourceFile.DocID, targetBatchNumber+1)
				}
				var wait sync.WaitGroup
				targetLength := len(targetFileIndexes)

				combinedAlignments := CombinedAlignments{sourceFile.DocID, []alignmentsPerDoc{}}
				c := make(chan []alignmentsPerDoc, config.numThreads)
				wait.Add(config.numThreads)
				start := 0
				increment := targetLength/config.numThreads + 1
				for i := 0; i < config.numThreads; i++ {
					end := increment * (i + 1)
					if end > targetLength {
						end = targetLength - 1
					}
					splitTargets := targetFileIndexes[start:end]
					start += increment
					if start > targetLength-1 {
						start = targetLength - 1
					}
					go func(splitTargets []docIndex, sourceAgainstSource bool, sourceMetadata map[string]map[string]string, targetMetadata map[string]map[string]string, localSourceFilesDone map[string]bool, config matchingParams, commonNgrams map[int32]bool) {
						defer wait.Done()
						localAlignments := []alignmentsPerDoc{}
					innerTargetMatching:
						for _, targetFile := range splitTargets {
							if sourceMetadata[sourceFile.DocID][config.sortingField] > targetMetadata[targetFile.DocID][config.sortingField] {
								continue
							}
							if sourceAgainstSource {
								if sourceMetadata[sourceFile.DocID]["filename"] == targetMetadata[targetFile.DocID]["filename"] {
									continue innerTargetMatching
								} else if _, ok := localSourceFilesDone[targetFile.DocID]; ok {
									continue innerTargetMatching
								}
							}
							sourceTargetIntersection := getIntersection(&sourceFile, &targetFile)
							if int32(len(sourceTargetIntersection)) < config.minimumMatchingNgramsInDocs {
								continue
							}
							mostCommonNgrams := getMostCommonNgrams(sourceTargetIntersection, &config.banalNgrams, commonNgrams)
							var matches = []ngramMatch{}
							for ngram := range sourceTargetIntersection {
								for _, sourceNgramIndex := range sourceFile.Ngrams[ngram] {
									for _, targetNgramIndex := range targetFile.Ngrams[ngram] {
										matches = append(matches, ngramMatch{sourceNgramIndex, targetNgramIndex, ngram})
									}
								}
							}
							sort.Slice(matches, func(i, j int) bool {
								if matches[i].source[0] < matches[j].source[0] {
									return true
								} else if matches[i].source[0] > matches[j].source[0] {
									return false
								}
								return matches[i].target[0] < matches[j].target[0]
							})
							alignments := matchPassage(&sourceFile, &targetFile, matches, &config, mostCommonNgrams)
							beforeVal := len(alignments)
							if config.twoWayMatching {
								alignments = reverseMatch(&sourceFile, &targetFile, matches, &config, mostCommonNgrams, alignments)
							}
							afterVal := len(alignments)
							if beforeVal != afterVal {
								println(beforeVal, afterVal)
							}
							counts += len(alignments)
							if len(alignments) > 0 {
								localAlignments = append(localAlignments, alignmentsPerDoc{targetFile.DocID, alignments})
							}
						}
						c <- localAlignments
					}(splitTargets, sourceAgainstSource, sourceMetadata, targetMetadata, localSourceFilesDone, config, commonNgrams)
				}
				wait.Wait()
				for i := 0; i < config.numThreads; i++ {
					localCombinedAlignments := <-c
					if len(localCombinedAlignments) > 0 {
						combinedAlignments.alignments = append(combinedAlignments.alignments, localCombinedAlignments...)
					}
				}
				if len(combinedAlignments.alignments) > 0 {
					writeAligments(&combinedAlignments, &sourceFile.DocID, sourceMetadata, targetMetadata, mergedOutput, &config, sourceFields, targetFields)
				}
				if sourceAgainstSource && sourceBatchNumber == targetBatchNumber {
					localSourceFilesDone[sourceFile.DocID] = true
				}
			}
			for sourceFileDocID := range localSourceFilesDone {
				sourceFilesDone[sourceFileDocID] = true
			}
			debug.FreeOSMemory()
		}
	}
	mergedOutput.Sync()
	mergedOutput.Close()
	fmt.Printf("%d results...\n", counts)
}
