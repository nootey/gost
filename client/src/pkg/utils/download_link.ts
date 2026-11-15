// startDownload has the browser load url as a download. The server names the file in its Content-Disposition header.
export function startDownload(url: string): void {
    const link = document.createElement("a");
    link.href = url;
    link.download = "";
    link.click();
}
