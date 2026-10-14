import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./App";
import "./styles/index.css";

const root = document.getElementById("root");
if (root === null) {
  throw new Error("The #root element is missing from the document");
}

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
