import { useEffect, useRef, useState } from "react";
import { message } from "./api";

export function useLoad<T>(key: string, load: () => Promise<T>) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const [version, setVersion] = useState(0);
  const loader = useRef(load);
  loader.current = load;
  useEffect(() => {
    let live = true;
    loader.current().then(
      (d) => {
        if (live) {
          setData(d);
          setError(undefined);
        }
      },
      (e) => {
        if (live) setError(message(e));
      },
    );
    return () => {
      live = false;
    };
  }, [key, version]);
  return { data, error, reload: () => setVersion((v) => v + 1) };
}
