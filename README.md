# scrip
SoundCloud track downloader. Injects metadata (author, title, genre, cover) into the audio. Can also download entire playlists/users

# Installing
You need to have [Go](https://go.dev) installed.
```sh
go install github.com/laptopcat/scrip@latest
```

*You might need to add go binaries to your PATH (add this line to your .bashrc / .zshrc / whatever)*
```sh
export PATH=${PATH}:`go env GOPATH`/bin
```

# Usage
- Download single track:
```sh
scrip https://soundcloud.com/<user>/<track>
```
This will output the audio to `<track>.mp3` in the current directory.

- Download playlist / album:
```sh
scrip https://soundcloud.com/<user>/sets/<playlist>
```
This will create a folder named `<playlist>`, and put the tracks as files named `<track>.mp3` into it.

- Download user:
```sh
scrip https://soundcloud.com/<user>
```
This will create a folder named `<user>`, and put the user's tracks as files named `<track>.mp3` into it.
