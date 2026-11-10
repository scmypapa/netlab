type Host = {
  basePath: string;
  theme?: "light" | "dark";
  environmentOnly?: boolean;
};

// The reverse proxy supplies this configuration alongside the application document.
const configuration = document.getElementById("netlab-host")?.textContent;
export const host: Host | undefined = configuration
  ? JSON.parse(configuration)
  : undefined;
export const basePath = host?.basePath.replace(/\/$/, "") ?? "";
export const apiPath = (path: string) => `${basePath}/api/v1${path}`;
