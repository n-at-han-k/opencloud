@skipOnReva
Feature: HLS streaming of videos through the webdav API
  As a user
  I want to stream a stored video as HLS
  So that I can watch it in a browser without downloading it first

  Background:
    Given user "Alice" has been created with default attributes


  # testvideo.mp4 already satisfies the output contract: H.264 High at level 4.1,
  # yuv420p, with AAC-LC stereo. Nothing needs re-encoding, so its packets are
  # copied and the segment boundaries fall on the source's own keyframes.
  Scenario Outline: stream a video whose codecs the browser already plays
    Given using <dav-path-version> DAV path
    And user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" downloads the HLS artifact "master.m3u8" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be an HLS multivariant playlist
    And the HLS multivariant playlist should offer codecs "avc1.640029,mp4a.40.2"
    Examples:
      | dav-path-version |
      | old              |
      | new              |
      | spaces           |


  # Nothing in testvideo.mkv plays in a browser: HEVC in 10-bit, with 5.1 AC-3.
  # Every stream is re-encoded, and the playlist must advertise the fixed output
  # contract rather than anything about the source.
  Scenario: stream a video the browser cannot play at all
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mkv" to "/video.mkv"
    When user "Alice" downloads the HLS artifact "master.m3u8" of "/video.mkv" using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be an HLS multivariant playlist
    And the HLS multivariant playlist should offer codecs "avc1.640029,mp4a.40.2"


  Scenario: get the media playlist of a copied video
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" downloads the HLS artifact "v0/index.m3u8" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "200"
    # 12 seconds of video, keyframes every 2s, cut at the first keyframe at or
    # past the 4s target: 0-4, 4-8, 8-12
    And the response should be an HLS media playlist with 3 segments


  Scenario: get the media playlist of a re-encoded video
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mkv" to "/video.mkv"
    When user "Alice" downloads the HLS artifact "v0/index.m3u8" of "/video.mkv" using the WebDAV API
    Then the HTTP status code should be "200"
    # 6 seconds re-encoded on a fixed 4s grid, because we place the keyframes
    And the response should be an HLS media playlist with 2 segments


  Scenario: get the initialization segment
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" downloads the HLS artifact "v0/init.mp4" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be a valid HLS initialization segment


  Scenario: get a media segment
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" downloads the HLS artifact "v0/seg00000.m4s" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be a valid HLS media segment


  Scenario: seek straight to the last segment without fetching the earlier ones
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" downloads the HLS artifact "v0/seg00002.m4s" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be a valid HLS media segment


  # The scenario that tests the video rather than the paperwork: walk the whole
  # stream as a player does and check every segment's own decode time puts it
  # where the playlist promised. A pipeline that answers 200 everywhere but
  # writes the same decode time into every segment passes every other scenario
  # here and plays four seconds on a loop.
  Scenario Outline: the whole stream plays as one continuous video
    Given using <dav-path-version> DAV path
    And user "Alice" has uploaded file "filesForUpload/<source>" to "/<source>"
    Then the HLS stream of "/<source>" for user "Alice" should be one continuous video of "<duration>" seconds
    Examples:
      | dav-path-version | source        | duration |
      | old              | testvideo.mp4 | 12       |
      | new              | testvideo.mp4 | 12       |
      | spaces           | testvideo.mp4 | 12       |
      | spaces           | testvideo.mkv | 6        |


  Scenario: stream a video shared by a public link
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    And user "Alice" has created the following resource link share:
      | resource        | video.mp4 |
      | space           | Personal  |
      | permissionsRole | view      |
    When the public downloads the HLS artifact "master.m3u8" from the last created public link using the WebDAV API
    Then the HTTP status code should be "200"
    And the response should be an HLS multivariant playlist


  Scenario Outline: ask for an artifact that is not one of the four
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" tries to download the HLS artifact "<artifact>" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "400"
    Examples:
      | artifact            |
      | index.m3u8          |
      | master.m3u         |
      | v0/master.m3u8     |
      | v0/seg42.m4s       |
      | v0/seg000042.m4s   |
      | v0/seg00000.ts     |
      | v0/init.m4s        |
      | v/index.m3u8       |
      | V0/index.m3u8      |


  # The artifact selector reaches a cache key, and the file cache turns a key
  # into a path. It must never be able to name anything outside its own prefix.
  Scenario Outline: the artifact selector cannot escape its namespace
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" tries to download the HLS artifact "<artifact>" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "400"
    Examples:
      | artifact                           |
      | ../../../etc/passwd                |
      | v0/../../../etc/seg00001.m4s       |
      | v0/x/../../../../seg00001.m4s      |
      | /v0/seg00001.m4s                   |


  Scenario: ask for a segment the playlist never offered
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" tries to download the HLS artifact "v0/seg09999.m4s" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "400"


  Scenario: ask for a rendition that does not exist
    Given user "Alice" has uploaded file "filesForUpload/testvideo.mp4" to "/video.mp4"
    When user "Alice" tries to download the HLS artifact "v9/index.m3u8" of "/video.mp4" using the WebDAV API
    Then the HTTP status code should be "400"


  Scenario Outline: ask for HLS of something that is not a video
    Given user "Alice" has uploaded file "filesForUpload/<source>" to "/<target>"
    When user "Alice" tries to download the HLS artifact "master.m3u8" of "/<target>" using the WebDAV API
    Then the HTTP status code should be "404"
    Examples:
      | source         | target     |
      | lorem.txt      | lorem.txt  |
      | testavatar.jpg | avatar.jpg |
      | simple.pdf     | simple.pdf |


  Scenario: ask for HLS of a file that does not exist
    When user "Alice" tries to download the HLS artifact "master.m3u8" of "/missing.mp4" using the WebDAV API
    Then the HTTP status code should be "404"


  Scenario: ask for HLS of a folder
    Given user "Alice" has created folder "/videos"
    When user "Alice" tries to download the HLS artifact "master.m3u8" of "/videos" using the WebDAV API
    Then the HTTP status code should be "400"
