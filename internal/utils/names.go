package utils

import (
	"fmt"
	"gost/internal/apperr"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxNameLength = 255

type NameKind int

const (
	FileName NameKind = iota
	FolderName
)

var nameLabels = map[NameKind]string{
	FileName:   "file name",
	FolderName: "folder name",
}

var nameKinds = map[bool]NameKind{
	false: FileName,
	true:  FolderName,
}

func NameKindOf(isFolder bool) NameKind {
	return nameKinds[isFolder]
}

func nameError(kind NameKind, problem string) error {
	return apperr.New(apperr.KindValidation, nameLabels[kind]+" "+problem)
}

// ValidateName checks a file or folder name. Slashes, "." and ".." are rejected,
// so a name can never act as a path.
func ValidateName(name string, kind NameKind) error {
	if strings.TrimSpace(name) == "" {
		return nameError(kind, "is required")
	}
	if !utf8.ValidString(name) || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return nameError(kind, "has invalid characters")
	}
	if utf8.RuneCountInString(name) > MaxNameLength {
		return nameError(kind, "is too long")
	}
	return nil
}

// SplitName splits a file name into base and extension at the last dot.
// Folders and dotfiles such as ".env" have no extension.
func SplitName(name string, kind NameKind) (base, ext string) {
	dot := strings.LastIndex(name, ".")
	if kind == FolderName || dot <= 0 || dot == len(name)-1 {
		return name, ""
	}
	return name[:dot], name[dot:]
}

// FreeName returns name, or the first numbered form of it that is not in taken.
// The number goes before the extension: "a.txt" becomes "a (1).txt".
func FreeName(name string, kind NameKind, taken []string) (string, error) {
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[t] = true
	}
	if !used[name] {
		return name, nil
	}

	base, ext := SplitName(name, kind)
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, n, ext)
		if utf8.RuneCountInString(candidate) > MaxNameLength {
			return "", nameError(kind, "is too long")
		}
		if !used[candidate] {
			return candidate, nil
		}
	}
}

// TreeFolder is one folder of a folder tree given as paths, such as "Photos/2024".
type TreeFolder struct {
	Path string
	// Parent is the path of the folder above, "" on top.
	Parent string
	Name   string
}

// TreeFolders checks the folder paths of a tree and returns its folders level by level, top first, each level sorted
// by path, so each folder comes after the folder above it. The folders above a path count even when the paths leave
// them out. A bad name gives the ValidateName error, and more than maxFolders folders gives a validation error.
func TreeFolders(paths []string, maxFolders int) ([]TreeFolder, error) {
	if len(paths) == 0 {
		return nil, apperr.New(apperr.KindInvalid, "invalid paths")
	}

	seen := make(map[string]bool)
	var levels [][]TreeFolder
	for _, path := range paths {
		names := strings.Split(path, "/")
		for depth, name := range names {
			sub := strings.Join(names[:depth+1], "/")
			if seen[sub] {
				continue
			}
			if err := ValidateName(name, FolderName); err != nil {
				return nil, err
			}
			seen[sub] = true
			if len(seen) > maxFolders {
				return nil, apperr.New(apperr.KindValidation, fmt.Sprintf("a folder upload can make at most %d folders", maxFolders))
			}
			if depth == len(levels) {
				levels = append(levels, nil)
			}
			levels[depth] = append(levels[depth], TreeFolder{Path: sub, Parent: strings.Join(names[:depth], "/"), Name: name})
		}
	}
	for _, level := range levels {
		slices.SortFunc(level, func(a, b TreeFolder) int { return strings.Compare(a.Path, b.Path) })
	}
	return slices.Concat(levels...), nil
}
