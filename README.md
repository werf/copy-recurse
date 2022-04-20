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
    UID: &uid,
    GID: &gid,
    MatchDir: func(path string) (copyrec.DirAction, error) {
        switch filepath.Base(path) {
        case "match-this-dir-fully":
            return copyrec.DirMatch, nil
        case "skip-this-dir":
            return copyrec.DirSkip, nil
        default:
            return copyrec.DirFallThrough, nil
        }
    },
    MatchFile: func(path string) (bool, error) {
        return filepath.Base(path) == "match-only-this-filename", nil
    },
})
if err != nil {
    return err
}

copyRec.Run(ctx)
```
