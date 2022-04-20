# Effective recursive file copying

## Usage

Basic:
```go
copyRec, err := copyrec.New(src, dest, copyrec.Options{})
if err != nil {
	return err
}

copyRec.Run(ctx)
```

Advanced:
```go
uid, gid := uint32(1000), uint32(1000)

copyRec, err := copyrec.New(src, dest, copyrec.Options{
    UID: uid,
	GID: gid,
    MatchDir: func(path string) (bool, error) {
        return filepath.Base(filepath.Clean(path)) == "some-matched-dir", nil
    },
    MatchFile: func(path string) (bool, error) {
        return true, nil
    },
    FallThroughDir: func(path string) (bool, error) {
        return true, nil
    },
})
if err != nil {
	return err
}

copyRec.Run(ctx)
```