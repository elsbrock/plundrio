# 🌊 plundrio

<p align="center">
<br/><br/>
<i>
Sailing the digital seas with ease,<br/>
Fetching treasures as we please.<br/>
With *arr at helm and put.io's might,<br/>
Downloads flow through day and night.
</i>
<br/><br/>
</p>

plundrio (`/ˈplʌndriˌoʊ/`) is a put.io download client designed to seamlessly
integrate with the *arr stack (Sonarr, Radarr, Lidarr, etc.). Files are
automatically added to put.io and downloaded to the local disk once complete.

## 🚀 Features

- 🔄 Seamless integration with Sonarr, Radarr, and other *arr applications
- ⚡ Fast and efficient downloads from put.io
- 🎯 Automatic download management and organization
- 🛠️ Easy configuration and setup
- 📊 Download progress monitoring

## 🔧 How It Works

plundrio makes downloading from put.io simple and automatic:

1. **Automatic Downloads**:
   - When your *arr apps (like Sonarr or Radarr) request new content, plundrio handles it automatically
   - No need to manually download files from put.io anymore

2. **Parallel Downloads**:
   - Downloads multiple files at the same time
   - You control how many downloads run simultaneously through configuration
   - Efficiently uses your available bandwidth

3. **File Management**:
   - Keeps track of what's already downloaded
   - Automatically downloads any missing files
   - Makes sure your download folder stays organized

## 📦 Installation

### Using Go

```bash
go install github.com/elsbrock/plundrio/cmd/plundrio@latest
```

### Using Docker

```bash
docker run --rm -it ghcr.io/elsbrock/plundrio:latest -- --help
```

Make sure to expose the transmission RPC port (9091) and mount the download directory.

### From Releases

Download the latest binary for your platform from the [releases page](https://github.com/elsbrock/plundrio/releases).

## 💡 Tips

- We recommend turning off the trash bin in your put.io settings. This helps keep your put.io account clean and saves space. The trash cannot be deleted programmatically.
- Downloads are throttled by put.io to 20MB/s. This means that if you have multiple downloads running at the same time, they will each download at 20MB/s. By default we run 4 downloads at a time - if you have more bandwidth, you can increase this number to saturate it.
- If you set the default download folder of put.io to the folder configured in plundrio, you can automatically download files added eg. via chill.institute.

## 🎮 Commands

### Run the download manager

plundrio run \
  --target /path/to/downloads \
  --folder "plundrio" \
  --token YOUR_PUTIO_TOKEN \
  --workers 4


### Generate configuration file

plundrio generate-config


### Get OAuth token

plundrio get-token


## ⚙️ Configuration

plundrio supports multiple configuration methods:

1. **Config file** (YAML format):

target: /path/to/downloads       # Target directory for downloads
folder: "plundrio"					# Folder name on put.io
token: "" 								# Get a token with get-token
listen: ":9091"						# Transmission RPC server address
workers: 4								# Number of download workers
earlyDelete: false					# Delete files immediately on download


2. **Command-line flags** (see full list with `plundrio run --help`)
3. **Environment variables** (prefixed with PLDR_):

export PLDR_TARGET=/path/to/downloads
export PLDR_TOKEN=your-putio-token


💡 **Security Note**: Avoid storing OAuth tokens in config files - use environment variables instead.

## 🔌 Configuring *arr Applications

To add plundrio to your *arr application (Sonarr, Radarr, etc.):

1. Go to Settings > Download Clients
2. Click the + button to add a new client
3. Select "Transmission" from the list
4. Fill in the following details:
   - Name: plundrio (or any name you prefer)
   - Host: localhost (or your server IP)
   - Port: 9091 (or your configured port)
   - URL Base: leave empty
   - Username: leave empty
   - Password: leave empty
5. Click "Test" to verify the connection
6. Save if the test is successful

plundrio will now automatically handle downloads from your *arr application through put.io.

## 🤝 Contributing

Pull requests are welcome! Please open an issue first to discuss what you would like to change.

## 📜 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
