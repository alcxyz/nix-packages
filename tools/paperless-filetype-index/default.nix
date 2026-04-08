{ lib, writeShellApplication, docker }:

writeShellApplication {
  name = "paperless-filetype-index";

  runtimeInputs = [ docker ];

  text = ''
    usage() {
      echo "Usage: paperless-filetype-index [--media-volume NAME] [--index-volume NAME]"
      echo ""
      echo "Builds a symlink index of non-PDF Paperless files organized by type."
      echo "Runs a temporary Alpine container to create symlinks in the index volume."
      echo ""
      echo "Options:"
      echo "  --media-volume NAME   Docker volume with Paperless media (default: arq_media)"
      echo "  --index-volume NAME   Docker volume for the index output (default: paperless_filetype_index)"
      echo "  -h, --help            Show this help"
      exit 0
    }

    MEDIA_VOLUME="arq_media"
    INDEX_VOLUME="paperless_filetype_index"

    while [ $# -gt 0 ]; do
      case "$1" in
        --media-volume) MEDIA_VOLUME="$2"; shift 2 ;;
        --index-volume) INDEX_VOLUME="$2"; shift 2 ;;
        -h|--help) usage ;;
        *) echo "Unknown option: $1"; usage ;;
      esac
    done

    echo "Building filetype index..."
    echo "  Media volume: $MEDIA_VOLUME"
    echo "  Index volume: $INDEX_VOLUME"

    docker run --rm \
      -v "''${MEDIA_VOLUME}:/paperless-media:ro" \
      -v "''${INDEX_VOLUME}:/paperless-by-type" \
      alpine sh -c '
        set -eu

        ORIGINALS="/paperless-media/documents/originals"
        OUTPUT="/paperless-by-type"

        rm -rf "''${OUTPUT:?}"/*

        get_type() {
          case "$1" in
            jpg|jpeg|png|gif|bmp|tiff|tif|webp|svg) echo "Images" ;;
            doc|docx|odt) echo "Word" ;;
            xls|xlsx|ods|csv) echo "Spreadsheets" ;;
            ppt|pptx|odp) echo "PowerPoint" ;;
            txt|md) echo "Text" ;;
            eml|msg) echo "Email" ;;
            *) echo "Other" ;;
          esac
        }

        count=0
        find "$ORIGINALS" -type f ! -iname "*.pdf" | while IFS= read -r filepath; do
          ext="''${filepath##*.}"
          ext_lower="$(echo "$ext" | tr "[:upper:]" "[:lower:]")"
          type_dir="$(get_type "$ext_lower")"
          relpath="''${filepath#"$ORIGINALS"/}"
          target_dir="''${OUTPUT}/''${type_dir}/$(dirname "$relpath")"
          mkdir -p "$target_dir"
          ln -sf "$filepath" "''${target_dir}/$(basename "$filepath")"
          count=$((count + 1))
        done

        echo "Indexed $count non-PDF files"
      '
  '';

  meta = with lib; {
    description = "Build a symlink index of non-PDF Paperless files organized by type";
    license = licenses.mit;
    platforms = platforms.linux;
  };
}
