package cacheddownloader

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type CachedDownloader interface {
	Fetch(url *url.URL, cacheKey string) (io.ReadCloser, error)
}

type CachingInfoType struct {
	ETag         string
	LastModified string
}

type CachedFile struct {
	size        int64
	access      time.Time
	cachingInfo CachingInfoType
	filePath    string
}

type cachedDownloader struct {
	cachedPath     string
	uncachedPath   string
	maxSizeInBytes int64
	downloader     *Downloader
	lock           *sync.Mutex

	cachedFiles map[string]CachedFile
}

func New(cachedPath string, uncachedPath string, maxSizeInBytes int64, downloadTimeout time.Duration) *cachedDownloader {
	os.RemoveAll(cachedPath)
	os.MkdirAll(cachedPath, 0770)
	return &cachedDownloader{
		cachedPath:     cachedPath,
		uncachedPath:   uncachedPath,
		maxSizeInBytes: maxSizeInBytes,
		downloader:     NewDownloader(downloadTimeout),
		lock:           &sync.Mutex{},
		cachedFiles:    map[string]CachedFile{},
	}
}

func (c *cachedDownloader) Fetch(url *url.URL, cacheKey string) (io.ReadCloser, error) {
	//return c.fetchUncachedFile(url)
	if cacheKey == "" {
		return c.fetchUncachedFile(url)
	} else {
		cacheKey = fmt.Sprintf("%x", md5.Sum([]byte(cacheKey)))
		return c.fetchCachedFile(url, cacheKey)
	}
}

func (c *cachedDownloader) fetchUncachedFile(url *url.URL) (io.ReadCloser, error) {
	destinationFile, err := ioutil.TempFile(c.uncachedPath, "uncached")
	if err != nil {
		return nil, err
	}

	_, _, _, err = c.downloader.Download(url, destinationFile, CachingInfoType{})
	if err != nil {
		os.RemoveAll(destinationFile.Name())
		return nil, err
	}

	if runtime.GOOS == "windows" {
		destinationFileName := destinationFile.Name()
		runtime.SetFinalizer(destinationFile, func(f *os.File) { f.Close(); os.RemoveAll(destinationFileName) })
	} else {
		os.RemoveAll(destinationFile.Name()) //OK, 'cause that's how unix works
	}

	destinationFile.Seek(0, 0)

	return destinationFile, nil
}

func (c *cachedDownloader) fetchCachedFile(url *url.URL, cacheKey string) (io.ReadCloser, error) {
	c.recordAccessForCacheKey(cacheKey)

	path := c.pathForCacheKeyWithLock(cacheKey)

	//download the file to a temporary location
	tempFile, err := ioutil.TempFile(c.uncachedPath, cacheKey+"-")
	if err != nil {
		return nil, err
	}
	tempFileName := tempFile.Name()
	// use RemoveAll. It has a better behavior on Windows. OS.Remove will remove the dir of the file, if the file dosn't exist and the dir of the file is empty.
	defer os.RemoveAll(tempFileName) //OK, even if we return tempFile 'cause that's how UNIX works.

	didDownload, size, cachingInfo, err := c.downloader.Download(url, tempFile, c.cachingInfoForCacheKey(cacheKey))
	if err != nil {
		return nil, err
	}

	tempFile.Close()

	if didDownload {
		if cachingInfo.ETag == "" && cachingInfo.LastModified == "" {
			c.removeCacheEntryFor(cacheKey)
			path = tempFileName
		} else {
			c.setCachingInfoForCacheKey(cacheKey, cachingInfo)

			//make room for the file and move it in (if possible)
			path = c.moveFileIntoCache(cacheKey, tempFileName, size)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	_, err = f.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	if runtime.GOOS == "windows" {
		if path == tempFileName {
			runtime.SetFinalizer(f,
				func(fp *os.File) {
					fp.Close()
					os.RemoveAll(path)
				})
		} else {
			runtime.SetFinalizer(f,
				func(fp *os.File) {
					c.lock.Lock()
					defer c.lock.Unlock()

					fp.Close()

					cf := c.cachedFiles[cacheKey]
					if path != cf.filePath {
						os.RemoveAll(path)
					}
				})
		}
	}

	return f, nil
}

func (c *cachedDownloader) moveFileIntoCache(cacheKey string, sourcePath string, size int64) string {
	c.lock.Lock()
	defer c.lock.Unlock()

	if size > c.maxSizeInBytes {
		//file does not fit in cache...
		return sourcePath
	}

	usedSpace := int64(0)
	for ck, f := range c.cachedFiles {
		if ck != cacheKey {
			usedSpace += f.size
		}
	}

	for c.maxSizeInBytes < usedSpace+size {
		oldestAccessTime, oldestCacheKey := time.Now(), ""
		for ck, f := range c.cachedFiles {
			if ck != cacheKey {
				if f.access.Before(oldestAccessTime) {
					oldestCacheKey = ck
					oldestAccessTime = f.access
				}
			}
		}

		usedSpace -= c.cachedFiles[oldestCacheKey].size

		fp := c.pathForCacheKey(cacheKey)
		if fp != "" {
			os.RemoveAll(fp)
		}
		delete(c.cachedFiles, cacheKey)
	}

	cachePath := filepath.Join(c.cachedPath, filepath.Base(sourcePath))

	f := c.cachedFiles[cacheKey]
	f.size = size
	f.filePath = cachePath
	c.cachedFiles[cacheKey] = f

	os.Rename(sourcePath, cachePath)
	return cachePath
}

func (c *cachedDownloader) pathForCacheKey(cacheKey string) string {
	f := c.cachedFiles[cacheKey]
	return f.filePath
}

func (c *cachedDownloader) pathForCacheKeyWithLock(cacheKey string) string {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.pathForCacheKey(cacheKey)
}

func (c *cachedDownloader) removeCacheEntryFor(cacheKey string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	fp := c.pathForCacheKey(cacheKey)
	if fp != "" {
		os.Remove(fp)
	}
	delete(c.cachedFiles, cacheKey)
}

func (c *cachedDownloader) recordAccessForCacheKey(cacheKey string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	f := c.cachedFiles[cacheKey]
	f.access = time.Now()
	c.cachedFiles[cacheKey] = f
}

func (c *cachedDownloader) cachingInfoForCacheKey(cacheKey string) CachingInfoType {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.cachedFiles[cacheKey].cachingInfo
}

func (c *cachedDownloader) setCachingInfoForCacheKey(cacheKey string, cachingInfo CachingInfoType) {
	c.lock.Lock()
	defer c.lock.Unlock()
	f := c.cachedFiles[cacheKey]
	f.cachingInfo = cachingInfo
	c.cachedFiles[cacheKey] = f
}

func (c *cachedDownloader) setFilePathForCacheKey(cacheKey string, filePath string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	f := c.cachedFiles[cacheKey]
	f.filePath = filePath
	c.cachedFiles[cacheKey] = f
}
