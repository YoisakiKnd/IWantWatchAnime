package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// Meta 是可选的刮削结果，由上层查询后注入。
type Meta struct {
	ID       string
	Title    string
	TitleCN  string
	Summary  string
	Date     string
	Poster   string
	Score    float64
	Episodes int
}

// Input 是一次整理请求。
type Input struct {
	TaskID int64
	Source string // 下载器给出的落点：文件或目录
	Parsed matcher.Parsed
	Meta   *Meta
	Group  string // 订阅分组，作为库内一级目录
}

// Result 是整理结果。
type Result struct {
	Path      string
	Linked    bool // true = 硬链接，占用 0 额外空间
	NFOPath   string
	TVNFOPath string // 番剧级元数据（tvshow.nfo），一季写一次
	Poster    string // 海报落点（poster.jpg），媒体库直接显示
}

// Organizer 执行「找正片 → 算目标路径 → 链接/复制 → 写 NFO」。
type Organizer struct {
	root       string
	rule       string
	mode       string
	writeNFO   bool
	seasonDir  bool
	minFreeGiB int64
	log        *slog.Logger
}

// New 构造整理器。
func New(cfg config.Library, log *slog.Logger) *Organizer {
	mode := cfg.LinkMode
	if mode == "" {
		mode = "hardlink"
	}
	return &Organizer{
		root:       cfg.Root,
		rule:       cfg.NameRule,
		mode:       mode,
		writeNFO:   cfg.WriteNFO,
		seasonDir:  cfg.SeasonDir,
		minFreeGiB: cfg.MinFreeGiB,
		log:        log,
	}
}

// 复制缓冲区从池里取，避免每个任务都分配 1MiB。
var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// Organize 执行整理。
//
// 关键行为：
//   - hardlink 失败（跨盘、exFAT/NTFS 不支持硬链接、权限不足）时自动降级为复制，
//     并把降级原因写进日志——玩客云的外接盘大概率是 exFAT 或 NTFS。
//   - 目标已存在且与源是同一个 inode 时直接返回，重复对账不会重复拷贝。
func (o *Organizer) Organize(ctx context.Context, in Input) (Result, error) {
	if in.Source == "" {
		return Result{}, errors.New("下载器未给出落点路径")
	}
	src, err := pickVideo(in.Source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("在 %s 里找不到视频文件（可能还没下完或目录已被清理）", in.Source)
		}
		return Result{}, err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return Result{}, err
	}

	p := in.Parsed
	v := Vars{
		Title:      firstNonEmpty(in.Meta.titleCN(), p.Title),
		Group:      in.Group,
		Fansub:     p.Fansub,
		Resolution: p.Resolution,
		Codec:      p.Codec,
		Source:     p.Source,
		Kind:       string(p.Kind),
		Season:     p.Season,
		Episode:    p.Episode,
		EpisodeTo:  p.EpisodeTo,
		Batch:      p.Batch,
	}
	if v.Group == "" {
		v.Group = "未分组"
	}

	rel := Render(o.rule, v)
	// 模板没写扩展名时补上，写了就不重复。
	if filepath.Ext(rel) == "" {
		rel += strings.ToLower(filepath.Ext(src))
	}
	dst := filepath.Join(o.root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Result{}, fmt.Errorf("创建目录 %s: %w", filepath.Dir(dst), err)
	}

	linked, err := o.place(src, dst, srcInfo.Size())
	if err != nil {
		return Result{}, err
	}

	res := Result{Path: dst, Linked: linked}
	if o.writeNFO {
		nfoPath := strings.TrimSuffix(dst, filepath.Ext(dst)) + ".nfo"
		metaID, plot, aired, rating := "", "", "", 0.0
		showTitle := v.Title
		if in.Meta != nil {
			metaID, plot, aired = in.Meta.ID, in.Meta.Summary, in.Meta.Date
			rating = in.Meta.Score
			showTitle = firstNonEmpty(in.Meta.titleCN(), p.Title)
		}
		if p, err := writeNFO(nfoPath, model.LibraryEntry{Title: v.Title, Episode: p.Episode},
			showTitle, p.Season, plot, aired, rating, metaID); err == nil {
			res.NFOPath = p
		} else {
			o.log.Warn("写 NFO 失败", "path", nfoPath, "err", err)
		}
		if in.Meta != nil {
			// 番剧级文件（tvshow.nfo / poster.jpg）与集数无关，一季只写一次。
			// 这里的失败不算入库失败：剧集本身已经放好了，媒体库只是少张封面。
			tv, poster, err := o.writeSeries(dst, v.Title, in.Meta)
			if err != nil {
				o.log.Warn("写番剧级元数据失败", "番剧", v.Title, "err", err)
			}
			res.TVNFOPath, res.Poster = tv, poster
		}
	}
	return res, nil
}

// writeSeries 在番剧根目录写 tvshow.nfo 与 poster.jpg。
//
// 同一部番剧的每一集都会走到这里，所以两个文件都不能无脑重写：
//   - poster.jpg「存在即跳过」——海报是张图片，重拷一遍纯属磨损存储卡；
//   - tvshow.nfo「内容变了才写」——它的内容来自刮削，而刮削可能比入库晚
//     （简介/评分走 Bangumi，入库那一刻可能还没回来），比对后再落盘，
//     老库才能在下一次入库时自动补上 plot/rating，不用人工删文件。
func (o *Organizer) writeSeries(dst, fallbackTitle string, m *Meta) (tvPath, posterPath string, err error) {
	dir := seriesDirOf(dst, o.seasonDir)
	title := firstNonEmpty(firstNonEmpty(m.TitleCN, m.Title), fallbackTitle)
	return o.seriesMeta(dir, title, m)
}

// RefreshSeries 给「已经入库」的番剧重写一遍番剧级元数据。
//
// 用在刮削补齐之后：入库时还没有简介/评分（或者当时还没配 Bangumi），
// 磁盘上就留着一份半成品 NFO；这里按库里存的某条文件路径反推出番剧根目录，
// 把它对齐，不必等下一集。
func (o *Organizer) RefreshSeries(libraryPath string, m *Meta) (tvPath, posterPath string, err error) {
	if o == nil || m == nil || libraryPath == "" {
		return "", "", nil
	}
	title := firstNonEmpty(m.TitleCN, m.Title)
	if title == "" {
		return "", "", nil
	}
	return o.seriesMeta(seriesDirOf(libraryPath, o.seasonDir), title, m)
}

// seriesMeta 是番剧级元数据的落盘实现，writeSeries 与 RefreshSeries 共用。
func (o *Organizer) seriesMeta(dir, title string, m *Meta) (tvPath, posterPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}

	tvPath = filepath.Join(dir, "tvshow.nfo")

	// 先把海报落到本地，再决定 NFO 里要不要写 <thumb>。
	// 反过来做会留一条死引用：NFO 说海报是 poster.jpg，目录里却没有那个文件，
	// Jellyfin/Kodi 每次打开都在找一张不存在的图。写进去就必须真的有。
	posterPath = o.placeSeriesPoster(dir, m)
	thumb := ""
	if posterPath != "" {
		thumb = filepath.Base(posterPath)
	}

	want, err := renderTVShowNFO(title, m.Title, m.Summary, m.Date, m.ID, m.Score, thumb)
	if err != nil {
		return "", "", err
	}
	if old, err := os.ReadFile(tvPath); err != nil || !bytes.Equal(old, want) {
		if _, err := writeTVShowNFO(tvPath, title, m.Title, m.Summary, m.Date, m.ID, m.Score, thumb); err != nil {
			return "", "", err
		}
	}
	return tvPath, posterPath, nil
}

// placeSeriesPoster 把番剧海报放进番剧根目录，返回落点（失败或没有海报时为空串）。
//
// 海报源是本地文件（蜜柑封面缓存）；只有 Bangumi 有封面时，pipeline 会先把它
// 下成文件再交过来，所以这里仍然只处理本地路径。
func (o *Organizer) placeSeriesPoster(dir string, m *Meta) string {
	if m.Poster == "" {
		return ""
	}
	dst := filepath.Join(dir, "poster.jpg")
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst // 已经有一张了，不重复拷
	}
	src, err := os.Stat(m.Poster)
	if err != nil {
		// 海报源文件（封面缓存）不在了：不算错误，媒体库少张图而已。
		o.log.Debug("海报源不在，跳过海报", "源", m.Poster, "err", err)
		return ""
	}
	if err := o.copyFile(m.Poster, dst, src.Size()); err != nil {
		return ""
	}
	return dst
}

// seriesDirOf 算出番剧根目录。
//
// 命名模板默认是 {group}/{title}/第 01 季/xxx.mkv，番剧级文件要落在
// {group}/{title}/ 下（第 01 季 的上一层）。模板没写季目录时就直接用文件所在目录。
func seriesDirOf(dst string, seasonDir bool) string {
	dir := filepath.Dir(dst)
	if !seasonDir {
		return dir
	}
	if seasonDirRe.MatchString(filepath.Base(dir)) {
		return filepath.Dir(dir)
	}
	return dir
}

// seasonDirRe 匹配「第 01 季 / 第1季 / Season 2 / S03」这类季目录名。
var seasonDirRe = regexp.MustCompile(`^(第\s*\d+\s*[季期]|[Ss]eason\s*\d+|[Ss]\d+)$`)

// place 把源文件放到目标位置，返回是否使用了硬链接。
func (o *Organizer) place(src, dst string, size int64) (bool, error) {
	if dstInfo, err := os.Stat(dst); err == nil {
		if srcInfo, err2 := os.Stat(src); err2 == nil && os.SameFile(dstInfo, srcInfo) {
			return dstInfo.Size() == srcInfo.Size(), nil // 已是同一份文件
		}
		if dstInfo.Size() == size {
			return false, nil // 大小一致视为已完成，避免重复搬运
		}
		dst = uniqueName(dst)
	}

	switch o.mode {
	case "move":
		if err := os.Rename(src, dst); err == nil {
			return false, nil
		}
		return false, o.copyThenRemove(src, dst, size)
	case "copy":
		return false, o.copyFile(src, dst, size)
	default: // hardlink
		linkErr := os.Link(src, dst)
		if linkErr == nil {
			return true, nil
		}
		if !fallbackToCopy(linkErr) {
			return false, fmt.Errorf("硬链接失败: %w", linkErr)
		}
		o.log.Info("该文件系统不支持硬链接，降级为复制",
			"src", src, "dst", dst, "reason", linkErr.Error())
		return false, o.copyFile(src, dst, size)
	}
}

// fallbackToCopy 判断硬链接失败是否属于「文件系统能力问题」。
func fallbackToCopy(err error) bool {
	return errors.Is(err, syscall.EXDEV) || // 跨设备
		errors.Is(err, syscall.EPERM) || // 部分挂载选项禁用硬链接
		errors.Is(err, syscall.ENOTSUP) || // exFAT / FAT32
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EMLINK)
}

func (o *Organizer) copyFile(src, dst string, size int64) error {
	if err := o.ensureSpace(dst, size); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	if _, err := io.CopyBuffer(out, in, *bp); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (o *Organizer) copyThenRemove(src, dst string, size int64) error {
	if err := o.copyFile(src, dst, size); err != nil {
		return err
	}
	return os.Remove(src)
}

// ensureSpace 在复制前检查剩余空间。
// 玩客云的盘通常只有几十 GB，与其跑到一半 ENOSPC，不如提前给出可读的失败原因。
func (o *Organizer) ensureSpace(dst string, need int64) error {
	free, err := freeBytes(filepath.Dir(dst))
	if err != nil {
		return nil // 取不到就算了，不影响主流程
	}
	needWithMargin := need + need/10 + 64<<20
	if free < needWithMargin {
		return fmt.Errorf("剩余空间不足：需要约 %s，可用 %s", humanBytes(needWithMargin), humanBytes(free))
	}
	return nil
}

func uniqueName(dst string) string {
	ext := filepath.Ext(dst)
	base := strings.TrimSuffix(dst, ext)
	for i := 1; i < 100; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
	return dst
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (m *Meta) titleCN() string {
	if m == nil {
		return ""
	}
	return firstNonEmpty(m.TitleCN, m.Title)
}

// humanBytes 输出便于人读的体积。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
