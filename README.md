# dep-dl
Package downloading from Gopkg.lock.
Now you don't need to wait long initial `dep ensure`!

## Usage
```bash
go get -u github.com/take-cheeze/dep-dl
cd $GOPATH/src/$YOUR_PROJECT_PATH
$GOPATH/bin/dep-dl
```

## Options
- `-v` : verbose output
- `-p $num` : parallism of download

## Limitation
Only GitHub and Git is supported for downloading.
