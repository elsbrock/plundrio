# 🌊 plundrio

<p align="center">
<i>
Sailing the digital seas with ease,<br/>
Fetching treasures as we please.<br/>
With *arr at helm and Put.io's might,<br/>
Downloads flow through day and night.
</i>
</p>

plundrio (`/ˈplʌndriˌoʊ/`) is a Put.io download client designed to seamlessly
integrate with the *arr stack (Sonarr, Radarr, Lidarr, etc.). Files are
automatically added to put.io and downloaded to the local disk once complete.

## 🚀 Features

- 🔄 Seamless integration with Sonarr, Radarr, and other *arr applications
- ⚡ Fast and efficient downloads from Put.io
- 🎯 Automatic download management and organization
- 🛠️ Easy configuration and setup
- 📊 Download progress monitoring

## 🔧 How It Works

plundrio makes downloading from Put.io simple and automatic:

1. **Automatic Downloads**:
   - When your *arr apps (like Sonarr or Radarr) request new content, plundrio handles it automatically
   - No need to manually download files from Put.io anymore

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
go install github.com/elsbrock/plundrio/cmd/putioarr@latest
```

### From Releases

Download the latest binary for your platform from the [releases page](https://github.com/elsbrock/plundrio/releases).

## ⚙️ Configuration

1. Create a configuration file at `~/.config/plundrio/config.yaml`:

```yaml
putio:
  token: "your-put-io-token"

server:
  port: 9091
  host: "localhost"

download:
  directory: "/path/to/downloads"
  concurrent: 3
```

2. Configure your *arr application to use plundrio as a download client:
   - Host: `localhost` (or your server IP)
   - Port: `9091` (or your configured port)
   - Category: (optional) for organized downloads

## 💡 Tips

- We recommend turning off the trash bin in your Put.io settings
- This helps keep your Put.io account clean and saves space
- Trash cannot be deleted programmatically

## 🎮 Usage

Start the server:

```bash
plundrio
```

The server will begin listening for download requests from your *arr applications and manage Put.io downloads automatically.

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

plundrio will now automatically handle downloads from your *arr application through Put.io.

## 🤝 Contributing

Pull requests are welcome! Please open an issue first to discuss what you would like to change.

## 📜 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
