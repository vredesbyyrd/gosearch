package database

import (
	"errors"
	"log"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/karrick/godirwalk"
	trie "github.com/ozeidan/go-patricia/patricia"
	"github.com/ozeidan/gosearch/internal/config"
	"github.com/ozeidan/gosearch/internal/fanotify"
	"github.com/ozeidan/gosearch/internal/request"
	"github.com/ozeidan/gosearch/pkg/tree"
)

// Start starts the indexing and listens for file changes and requests
// changeSender is used to get file change messages from the caller
// requestSender is used to get request messages from the caller
func Start(changeSender <-chan fanotify.FileChange,
	requestSender <-chan request.Request) {
	initialIndex()

	for {
		select {
		case change := <-changeSender:
			refreshDirectory(change.FolderPath)
		case req := <-requestSender:
			queryIndex(req)
		}
	}
}

var errFilter = errors.New("directory filtered")

var indexTrie *trie.Trie = trie.NewTrie()
var fileTree *tree.Node = tree.New()

type indexedFile struct {
	pathNode *tree.Node
}

func initialIndex() {
	log.Println("starting to create initial index")
	start := time.Now()
	dirname := "/"
	files, directories := addToIndexRecursively(dirname)
	end := time.Now()
	log.Println("finished creating initial index")
	log.Printf("indexed %d files and %d directories in %f seconds",
		files, directories, end.Sub(start).Seconds())
	PrintMemUsage()
}

func refreshDirectory(path string) {
	log.Println("refreshing directory", path)
	newDirents, err := godirwalk.ReadDirents(path, nil)
	if err != nil {
		log.Println("warning: couldn't read directory", path, err)
	}

	newNames := make([]string, 0, len(newDirents))
	nameDirents := make(map[string]godirwalk.Dirent, len(newNames))
	for _, dirent := range newDirents {
		name := dirent.Name()
		if config.IsPathFiltered(filepath.Join(path, name)) {
			// log.Println("ignoring filtered file", name)
			continue
		}
		newNames = append(newNames, name)
		nameDirents[dirent.Name()] = *dirent
	}

	oldNames, err := fileTree.GetChildren(path)
	if err != nil {
		log.Println("couldn't get children of path", path, err)
	}

	createdNames, deletedNames := sliceDifference(newNames, oldNames)
	if len(createdNames) > 0 {
		log.Printf("indexing new files %v\n", createdNames)
	}
	if len(deletedNames) > 0 {
		log.Printf("removing deleted files %v from index", deletedNames)
	}

	for _, name := range createdNames {
		dirent := nameDirents[name]
		pathName := filepath.Join(path, name)
		if config.IsPathFiltered(pathName) {
			continue
		}
		addToIndex(path, name, dirent)
	}

	for _, name := range deletedNames {
		pathName := filepath.Join(path, name)
		deleteFromIndex(path, name)
		fileTree.DeleteAt(pathName)
	}
}

func sliceDifference(sliceA, sliceB []string) ([]string, []string) {
	mapA := sliceToSet(sliceA)
	mapB := sliceToSet(sliceB)

	for name := range mapA {
		if _, ok := mapB[name]; ok {
			delete(mapA, name)
			delete(mapB, name)
		}
	}

	return setToSlice(mapA), setToSlice(mapB)
}

func sliceToSet(slice []string) map[string]bool {
	createMap := make(map[string]bool, len(slice))
	for _, name := range slice {
		createMap[name] = true
	}
	return createMap
}

func setToSlice(set map[string]bool) []string {
	createSlice := make([]string, 0, len(set))

	for key := range set {
		createSlice = append(createSlice, key)
	}

	return createSlice
}

func addToIndex(path, name string, dirent godirwalk.Dirent) {
	pathName := filepath.Join(path, name)

	if dirent.IsDir() {
		addToIndexRecursively(pathName)
	} else {
		newNode := fileTree.Add(pathName)
		indexTrieAdd(name, indexedFile{newNode})
	}
}

func deleteFromIndex(path, name string) {
	pathName := filepath.Join(path, name)

	indexTrieDelete(name, path)
	children, err := fileTree.GetChildren(pathName)
	if err != nil {
		// fmt.Println("warning:", err)
		return
	}

	for _, child := range children {
		deleteFromIndex(pathName, child)
	}
}

func addToIndexRecursively(path string) (uint64, uint64) {
	var directoryCount uint64
	var fileCount uint64
	godirwalk.Walk(path, &godirwalk.Options{
		Callback: func(osPathname string, de *godirwalk.Dirent) error {
			if config.IsPathFiltered(osPathname) {
				return errFilter
			}

			if de.IsDir() {
				directoryCount++
			} else {
				fileCount++
			}

			newNode := fileTree.Add(osPathname)
			name := de.Name()
			newFile := indexedFile{newNode}
			indexTrieAdd(name, newFile)

			return nil
		},
		Unsorted: true,
		ErrorCallback: func(_ string, err error) godirwalk.ErrorAction {
			if err == errFilter {
				return godirwalk.SkipNode
			}
			// fmt.Println(err)
			return godirwalk.SkipNode
		},
	})

	return fileCount, directoryCount
}

func indexTrieAdd(name string, index indexedFile) {
	prefix := trie.Prefix(name)
	if item := indexTrie.Get(prefix); item != nil {
		fileList := item.([]indexedFile)
		fileList = append(fileList, index)
		indexTrie.Set(prefix, fileList)
	} else {
		indexTrie.Insert(prefix, []indexedFile{index})
	}
}

func indexTrieDelete(name, path string) {
	prefix := trie.Prefix(name)
	filePath := filepath.Join(path, name)
	if item := indexTrie.Get(prefix); item != nil {
		fileList := item.([]indexedFile)
		for i := 0; i < len(fileList); i++ {
			index := fileList[i]
			existingPath := index.pathNode.GetPath()

			if existingPath != filePath {
				continue
			}

			fileList[i] = fileList[len(fileList)-1]
			fileList = fileList[:len(fileList)-1]
			break
		}
		indexTrie.Set(prefix, fileList)
	}
}

type sortResult struct {
	result  string
	skipped int
}

type bySkipped []sortResult

func (a bySkipped) Len() int      { return len(a) }
func (a bySkipped) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a bySkipped) Less(i, j int) bool {
	if a[i].skipped == a[j].skipped {
		return len(a[i].result) < len(a[j].result)
	}
	return a[i].skipped < a[j].skipped
}

type byLength []string

func (a byLength) Len() int           { return len(a) }
func (a byLength) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byLength) Less(i, j int) bool { return len(a[i]) < len(a[j]) }

func queryIndex(req request.Request) {
	defer close(req.ResponseChannel)
	prefix := trie.Prefix(req.Query)

	switch req.Settings.Action {
	case request.PrefixSearch:
		fallthrough
	case request.SubStringSearch:
		var visitFunc func(trie.Prefix, trie.VisitorFunc) error
		if req.Settings.Action == request.PrefixSearch {
			visitFunc = indexTrie.VisitSubtree
		} else {
			visitFunc = indexTrie.VisitSubstring
		}

		if req.Settings.NoSort {
			visitFunc(prefix, sendResults(req.ResponseChannel))
			return
		}

		var results []string
		visitFunc(prefix, func(prefix trie.Prefix, item trie.Item) error {
			list := item.([]indexedFile)
			for _, file := range list {
				results = append(results, file.pathNode.GetPath())
			}
			return nil
		})

		// normal sorting is from worst to best
		// so that the best result will show right
		// above the command prompt
		if req.Settings.ReverseSort {
			sort.Sort(byLength(results))
		} else {
			sort.Sort(sort.Reverse(byLength(results)))
		}

		for _, result := range results {
			req.ResponseChannel <- result
		}
	case request.FuzzySearch:
		if req.Settings.NoSort {
			indexTrie.VisitFuzzy(
				prefix,
				func(prefix trie.Prefix, item trie.Item, skipped int) error {
					return sendResults(req.ResponseChannel)(prefix, item)
				})
			return
		}

		var results []sortResult
		visitor := func(prefix trie.Prefix, item trie.Item, skipped int) error {
			list := item.([]indexedFile)
			for _, file := range list {
				results = append(results, sortResult{file.pathNode.GetPath(), skipped})
			}
			return nil
		}
		indexTrie.VisitFuzzy(prefix, visitor)

		if req.Settings.ReverseSort {
			sort.Sort(bySkipped(results))
		} else {
			sort.Sort(sort.Reverse(bySkipped(results)))
		}

		for _, result := range results {
			req.ResponseChannel <- result.result
		}
	}
}

func sendResults(channel chan string) trie.VisitorFunc {
	return func(prefix trie.Prefix, item trie.Item) error {
		list := item.([]indexedFile)
		for _, file := range list {
			channel <- file.pathNode.GetPath()
		}
		return nil
	}
}

func PrintMemUsage() {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	log.Println("Memory Statistics:")
	log.Printf("\tAlloc = %v MiB", bToMb(m.Alloc))
	log.Printf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	log.Printf("\tSys = %v MiB", bToMb(m.Sys))
}
func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
